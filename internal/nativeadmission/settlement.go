package nativeadmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/jackc/pgx/v5"
)

// Attribution never replaces the workload's owner with caller-supplied userId.
func (a Admission) Attribution() contract.Attribution {
	authority := contract.ServiceAuthority(a.Principal.Authority)
	return contract.Attribution{RequestID: a.RequestID, UserID: a.UserID, Principal: contract.AuthenticatedPrincipal{Authority: &authority, Billing: contract.BillingPrincipal{AccountID: *a.Principal.OwnerAccountID}, ApplicationID: a.Principal.ApplicationID, CredentialID: a.Principal.CredentialID, Environment: a.Principal.Environment, InferenceScopes: a.Principal.Scopes}}
}

// Settle atomically records authoritative usage, provider costs and the release
// of capacity. A late report may settle an expired lease, never rerun its request.
func (s *Store) Settle(ctx context.Context, id string, report contract.UsageReport, events []providercost.Event) error {
	if err := report.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := readAdmission(ctx, tx, id)
	if err != nil {
		return err
	}
	if report.RequestID != a.RequestID || !sameAttribution(a.Attribution(), report.Attribution) {
		return ErrConflict
	}
	for _, event := range events {
		if event.RequestID != a.RequestID {
			return ErrConflict
		}
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	fingerprint, err := completionDigest(report, events)
	if err != nil {
		return err
	}
	var old string
	err = tx.QueryRow(ctx, `SELECT digest FROM kaana_admission.settlements WHERE admission_id=$1`, id).Scan(&old)
	if err == nil {
		if old != fingerprint {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT digest FROM kaana_admission.completions WHERE admission_id=$1`, id).Scan(&old)
	if err == nil && old != fingerprint {
		return ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	generation, ratingFailure := meteredRecord(*a, report)
	encoded, err := json.Marshal(generation)
	if err != nil {
		return err
	}
	if err = credentialstore.RecordProviderCostsInTransaction(ctx, tx, events); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.settlements(id,admission_id,digest,report,generation,outcome,rating_failure) VALUES($1,$2,$3,$4,$5,$6,$7)`, newID("kst_"), id, fingerprint, body, encoded, report.Outcome, ratingFailure); err != nil {
		return fmt.Errorf("nativeadmission: recording settlement: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE kaana_admission.admissions SET state='settled',generation_id=$2 WHERE id=$1`, id, report.GenerationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func readAdmission(ctx context.Context, tx pgx.Tx, id string) (*Admission, error) {
	var a Admission
	var principal, plan []byte
	err := tx.QueryRow(ctx, `SELECT id,request_id,principal,delegated_user_id,operation,plan,admitted_at,lease_until FROM kaana_admission.admissions WHERE id=$1 AND authority='kaana' FOR UPDATE`, id).Scan(&a.ID, &a.RequestID, &principal, &a.UserID, &a.Operation, &plan, &a.AdmittedAt, &a.LeaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(principal, &a.Principal); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(plan, &a.Plan); err != nil {
		return nil, err
	}
	return &a, nil
}

func sameAttribution(expected, actual contract.Attribution) bool {
	// generationId is minted by the executor; every identity field is immutable.
	expected.GenerationID = nil
	actual.GenerationID = nil
	e, err := json.Marshal(expected)
	if err != nil {
		return false
	}
	a, err := json.Marshal(actual)
	return err == nil && string(e) == string(a)
}

func meteredRecord(a Admission, r contract.UsageReport) (contract.MeteredGenerationRecord, *string) {
	units := append([]contract.UsageQuantity{}, r.Units...)
	record := contract.MeteredGenerationRecord{SchemaVersion: 1, Kind: "metered_usage", RequestID: a.RequestID, GenerationID: r.GenerationID, ApplicationID: a.Principal.ApplicationID, CredentialID: a.Principal.CredentialID, DelegatedUserID: a.UserID, Environment: a.Principal.Environment, Outcome: r.Outcome, UsageSource: r.UsageSource, Units: units, Model: r.ResolvedModelReference, SettledAt: r.CompletedAt, MeteredUsageID: a.ID, Tariff: contract.MeteredTariff{Status: "unpriced"}, CustomerCharge: contract.MeteredCustomerCharge{Status: "not_charged"}}
	reason := "model_not_priced"
	for _, price := range a.Plan.Prices {
		if price.ModelLine != r.ResolvedModelReference.ModelID() {
			continue
		}
		version := price.PriceVersionID
		record.Tariff.PriceVersionID = &version
		amount, err := RateUnits(price, r.Units)
		if err != nil {
			reason = "unpriced_unit"
			return record, &reason
		}
		record.Tariff = contract.MeteredTariff{Status: "quoted", Amount: &amount.Amount, Currency: &amount.Currency, PriceVersionID: &version}
		actual, _ := moneyRat(amount)
		ceiling, err := moneyRat(a.Plan.Quote)
		if err != nil || actual.Cmp(ceiling) > 0 {
			reason = "admission_quote_exceeded"
			return record, &reason
		}
		return record, nil
	}
	return record, &reason
}

// Generation reads only the same authenticated owner/application/environment
// and credential. Optional attribution further narrows key lookup; it never
// widens who owns the record. There is no financial fallback on a miss.
func (s *Store) Generation(ctx context.Context, principal contract.IdentityPrincipal, id *contract.GenerationID, key *contract.IdempotencyKey, user *contract.UserID) (json.RawMessage, error) {
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if principal.OwnerAccountID == nil {
		return nil, ErrNotEnrolled
	}
	if (id == nil) == (key == nil) {
		return nil, ErrInvalidRequest
	}
	var body []byte
	query := `SELECT s.generation FROM kaana_admission.admissions a JOIN kaana_admission.settlements s ON s.admission_id=a.id WHERE a.authority='kaana' AND a.principal->>'ownerAccountId'=$1 AND a.principal->>'applicationId'=$2 AND a.principal->>'environment'=$3 AND a.principal->>'credentialId'=$4`
	args := []any{principal.OwnerAccountID, principal.ApplicationID, principal.Environment, principal.CredentialID}
	if id != nil {
		query += ` AND (a.generation_id=$5 OR a.request_id=$5)`
		args = append(args, *id)
	} else {
		scope, _ := idempotencyIdentity(principal.CredentialID, key, "")
		query += ` AND a.idempotency_scope=$5 AND a.idempotency_key=$6 AND a.delegated_user_id IS NOT DISTINCT FROM $7::text`
		args = append(args, scope, *key, user)
	}
	err := s.pool.QueryRow(ctx, query, args...).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

// Expire marks incomplete leases explicitly. It never creates zero usage or
// releases idempotency; the request's final provider outcome is still unknown.
func (s *Store) Expire(ctx context.Context) (int64, error) {
	result, err := s.pool.Exec(ctx, `UPDATE kaana_admission.admissions SET state='abandoned' WHERE state='executing' AND lease_until<=clock_timestamp()`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}
