// Package commercialadmission joins native authorization and validated rating
// to the commercial ledger in the same PostgreSQL transaction. It is not wired
// into the serving process; no account, funding or policy is auto-enrolled.
package commercialadmission

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettlementGrace leaves a bounded recovery margin after the maximum admitted
// execution. Long commercial sessions need an explicit renewable-hold design;
// this coordinator refuses them before execution instead of shortening them.
const SettlementGrace = time.Minute

var ErrHoldLifetime = errors.New("commercialadmission: execution exceeds bounded hold lifetime")

//go:embed migrations/0001_coordinator.sql
var migration string

type Coordinator struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Coordinator { return &Coordinator{pool: pool} }

var _ nativeadmission.AdmissionEconomics = (*Coordinator)(nil)

// Migrate is a separate offline operation, after native and billing migrations.
// Its migration role requires REFERENCES on native admissions; runtime gets no
// migration, funding or account-management privileges.
func (c *Coordinator) Migrate(ctx context.Context) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-commercial-migrations')); CREATE TABLE IF NOT EXISTS public.kaana_commercial_migrations(version text PRIMARY KEY,checksum text NOT NULL); REVOKE ALL ON public.kaana_commercial_migrations FROM PUBLIC`); err != nil {
		return err
	}
	hash := digest([]byte(migration))
	var old string
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_commercial_migrations WHERE version='0001'`).Scan(&old)
	if err == nil {
		if old != hash {
			return billing.ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, migration); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_commercial_migrations VALUES('0001',$1)`, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func priceDigest(a nativeadmission.Admission) (string, error) {
	raw, err := json.Marshal(a.Plan.Prices)
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

func (c *Coordinator) Reserve(ctx context.Context, tx pgx.Tx, a nativeadmission.Admission) error {
	if a.Plan.EconomicTreatment != nativeadmission.Commercial || a.Principal.OwnerAccountID == nil {
		return nativeadmission.ErrNotEnrolled
	}
	duration := a.Plan.ExecutionTimeout
	if duration <= 0 || duration > 15*time.Minute-SettlementGrace {
		return ErrHoldLifetime
	}
	ttl := ((duration+time.Second-1)/time.Second)*time.Second + SettlementGrace
	account, err := billing.AccountForOwnerInTransaction(ctx, tx, string(*a.Principal.OwnerAccountID), string(a.Plan.Quote.Currency))
	if err != nil {
		return err
	}
	prices, err := priceDigest(a)
	if err != nil {
		return err
	}
	hold, err := billing.ReserveInTransaction(ctx, tx, billing.ReserveRequest{AccountID: account.ID, Key: "native:" + a.ID, AdmissionID: a.ID, PriceSnapshotID: "native-plan:" + prices, PriceDigest: prices, Maximum: string(a.Plan.Quote.Amount), TTL: ttl})
	if err != nil {
		return err
	}
	if hold.Replay {
		return billing.ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO kaana_commercial.admissions(admission_id,account_id,hold_id,policy_digest,price_digest) VALUES($1,$2,$3,$4,$5)`, a.ID, account.ID, hold.ID, a.Plan.PolicyDigest, prices)
	return err
}

func (c *Coordinator) Settle(ctx context.Context, tx pgx.Tx, r nativeadmission.RatedCompletion) (nativeadmission.EconomicSettlement, error) {
	unavailable := func(reason string) (nativeadmission.EconomicSettlement, error) {
		_, err := tx.Exec(ctx, `INSERT INTO kaana_commercial.outcomes(admission_id,evidence_digest,status) VALUES($1,$2,$3)`, r.Admission.ID, r.EvidenceDigest, reason)
		return nativeadmission.EconomicSettlement{UnavailableReason: reason}, err
	}
	a := r.Admission
	if a.Plan.EconomicTreatment != nativeadmission.Commercial || a.Principal.OwnerAccountID == nil {
		return nativeadmission.EconomicSettlement{}, nativeadmission.ErrNotEnrolled
	}
	account, err := billing.AccountForOwnerInTransaction(ctx, tx, string(*a.Principal.OwnerAccountID), string(a.Plan.Quote.Currency))
	if err != nil {
		return nativeadmission.EconomicSettlement{}, err
	}
	var accountID, holdID, policyHash, priceHash string
	if err = tx.QueryRow(ctx, `SELECT account_id,hold_id,policy_digest,price_digest FROM kaana_commercial.admissions WHERE admission_id=$1`, a.ID).Scan(&accountID, &holdID, &policyHash, &priceHash); err != nil {
		return nativeadmission.EconomicSettlement{}, err
	}
	prices, err := priceDigest(a)
	if err != nil {
		return nativeadmission.EconomicSettlement{}, err
	}
	if account.ID != accountID || policyHash != a.Plan.PolicyDigest || priceHash != prices {
		return nativeadmission.EconomicSettlement{}, billing.ErrConflict
	}
	if r.RatingFailure != nil {
		return unavailable("rating_failed")
	}
	if r.Metered.Tariff.Amount == nil || r.Metered.Tariff.Currency == nil {
		return nativeadmission.EconomicSettlement{}, nativeadmission.ErrUnpriced
	}
	served, priceErr := a.Plan.PriceForReport(r.Report)
	if priceErr != nil {
		return nativeadmission.EconomicSettlement{}, priceErr
	}

	hasUsage := false
	for _, u := range r.Report.Units {
		if u.Quantity > 0 {
			hasUsage = true
		}
	}
	result, err := billing.SettleInTransaction(ctx, tx, billing.SettleRequest{AccountID: accountID, HoldID: holdID, Key: "native-settle:" + a.ID, EvidenceID: "native-completion:" + a.ID, EvidenceDigest: r.EvidenceDigest, Outcome: string(r.Report.Outcome), HasUsage: hasUsage, RatedAmount: string(*r.Metered.Tariff.Amount)})
	if err != nil {
		return nativeadmission.EconomicSettlement{}, err
	}
	if result.State == "expired" {
		return unavailable("hold_expired")
	}
	if result.Receipt == nil {
		return nativeadmission.EconomicSettlement{}, billing.ErrConflict
	}
	receipt := result.Receipt
	snapshot := contract.PriceSnapshot{PriceVersionID: served.PriceVersionID, Currency: served.Currency, UnitPrices: make([]contract.UnitPrice, 0, len(served.UnitPrices))}
	for _, p := range served.UnitPrices {
		snapshot.UnitPrices = append(snapshot.UnitPrices, contract.UnitPrice{Unit: p.Unit, Amount: contract.ExactDecimal(p.Amount), Per: p.Per, Currency: p.Currency})
	}
	var settledAt time.Time
	if err = tx.QueryRow(ctx, `SELECT created_at FROM kaana_billing.receipts WHERE id=$1 AND account_id=$2`, receipt.ID, accountID).Scan(&settledAt); err != nil {
		return nativeadmission.EconomicSettlement{}, err
	}
	charge := &contract.ChargedGenerationRecord{SchemaVersion: 1, Kind: "charge", RequestID: a.RequestID, GenerationID: r.Report.GenerationID, ApplicationID: a.Principal.ApplicationID, CredentialID: a.Principal.CredentialID, DelegatedUserID: a.UserID, Environment: a.Principal.Environment, Outcome: r.Report.Outcome, UsageSource: r.Report.UsageSource, Units: append([]contract.UsageQuantity{}, r.Report.Units...), Model: r.Report.ResolvedModelReference, SettledAt: contract.Timestamp(settledAt.UTC().Format(time.RFC3339Nano)), ReceiptID: receipt.ID, PriceSnapshot: snapshot, BilledAmount: contract.ExactDecimal(receipt.Charged), Currency: served.Currency, PlatformFeeOnly: false}
	if err = charge.Validate(); err != nil {
		return nativeadmission.EconomicSettlement{}, err
	}
	status := "charged"
	if receipt.Charged == "0.000000000000" {
		status = "zero_charge"
	}
	_, err = tx.Exec(ctx, `INSERT INTO kaana_commercial.outcomes(admission_id,evidence_digest,status,receipt_id) VALUES($1,$2,$3,$4)`, a.ID, r.EvidenceDigest, status, receipt.ID)
	return nativeadmission.EconomicSettlement{Charge: charge}, err
}

// Recovery completes already staged evidence before expiring unknown requests.
// It never runs inference. Errors retain committed counts; repeated recovery is
// safe, and one failed rating/delivery cannot disable the hold expiry sweep.
type Recovery struct {
	Completed int
	Abandoned int64
	Released  int
}

func (c *Coordinator) Recover(ctx context.Context, native *nativeadmission.Store, limit int) (Recovery, error) {
	if limit < 1 || limit > 1000 {
		return Recovery{}, nativeadmission.ErrInvalidRequest
	}
	var result Recovery
	completed, e1 := native.ReplayCompletions(ctx, limit)
	result.Completed = completed
	abandoned, e2 := native.Expire(ctx)
	result.Abandoned = abandoned
	released, e3 := billing.NewStore(c.pool).Expire(ctx, limit)
	result.Released = released
	if err := errors.Join(e1, e2, e3); err != nil {
		return result, fmt.Errorf("commercialadmission: recovery: %w", err)
	}
	return result, nil
}
