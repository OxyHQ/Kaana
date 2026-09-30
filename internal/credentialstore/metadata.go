package credentialstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// CredentialMetadataSchemaVersion is the only accepted metadata document shape.
const CredentialMetadataSchemaVersion = 1

// CapacityCategory is what kind of capacity funds a key, as the operator states
// it. It is protected operator metadata and never selects a key: Oxy orders
// deployments, and Kaana executes the exact binding it was given.
type CapacityCategory string

const (
	CapacityTrial       CapacityCategory = "trial"
	CapacityPromotional CapacityCategory = "promotional"
	CapacityPrepaid     CapacityCategory = "prepaid"
	CapacityPaid        CapacityCategory = "paid"
)

// CommercialUse is whether this exact key may serve commercial traffic, as
// stated for it. It is never derived from the capacity category.
type CommercialUse string

const (
	CommercialUsePermitted    CommercialUse = "permitted"
	CommercialUseNotPermitted CommercialUse = "not_permitted"
	CommercialUseUnknown      CommercialUse = "unknown"
)

// CredentialMetadata is the whole protected operator description of one exact
// platform key. The key's identity remains (Provider, KeyID); nothing here is
// an identity, and none of it is readable by the inference runtime.
type CredentialMetadata struct {
	SchemaVersion    int                   `json:"schemaVersion"`
	OperationID      string                `json:"operationId"`
	Provider         contract.ProviderSlug `json:"provider"`
	KeyID            string                `json:"keyId"`
	Title            string                `json:"title"`
	FundingAccount   FundingAccount        `json:"fundingAccount"`
	CapacityCategory CapacityCategory      `json:"capacityCategory"`
	Environment      string                `json:"environment"`
	CommercialUse    CommercialUseClaim    `json:"commercialUse"`
	Restrictions     *KeyRestrictions      `json:"restrictions,omitempty"`
}

// FundingAccount is the upstream account paying for a key. Keys that name the
// same ID share capacity; the Label is protected text such as an account email.
type FundingAccount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// CommercialUseClaim is an eligibility and what it rests on.
type CommercialUseClaim struct {
	Eligibility CommercialUse `json:"eligibility"`
	Evidence    string        `json:"evidence,omitempty"`
}

// KeyRestrictions limits what a key may serve. A nil list is unrestricted; a
// present list must name at least one entry.
type KeyRestrictions struct {
	Models       []string `json:"models,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

var (
	metadataOperationPattern = regexp.MustCompile(`^kcm_[0-9a-f]{32}$`)
	fundingAccountPattern    = regexp.MustCompile(`^kfa_[0-9a-f]{32}$`)
	capacityEvidencePattern  = regexp.MustCompile(`^kce_[0-9a-f]{32}$`)
	capabilityPattern        = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	quotaUnitPattern         = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// keyIDPattern is the stored key id's shape (0011). Existence of the exact
	// row is what the database checks; this only refuses a malformed id early.
	keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// ParseCredentialMetadata decodes and validates one metadata document. Unknown
// fields are refused: a misspelled restriction would otherwise be dropped and
// leave the key unrestricted.
func ParseCredentialMetadata(raw []byte) (CredentialMetadata, error) {
	var metadata CredentialMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return CredentialMetadata{}, fmt.Errorf("credential store: credential metadata is not a valid document: %w", err)
	}
	if decoder.More() {
		return CredentialMetadata{}, errors.New("credential store: credential metadata has trailing content")
	}
	if err := ValidateCredentialMetadata(metadata); err != nil {
		return CredentialMetadata{}, err
	}
	return metadata, nil
}

// ValidateCredentialMetadata refuses an incomplete or ambiguous description.
func ValidateCredentialMetadata(metadata CredentialMetadata) error {
	switch {
	case metadata.SchemaVersion != CredentialMetadataSchemaVersion:
		return fmt.Errorf("credential store: credential metadata schemaVersion must be %d", CredentialMetadataSchemaVersion)
	case !metadataOperationPattern.MatchString(metadata.OperationID):
		return errors.New("credential store: credential metadata operationId must be kcm_ and 32 lowercase hex digits")
	case !metadata.Provider.Valid():
		return errors.New("credential store: credential metadata provider is invalid")
	case !keyIDPattern.MatchString(metadata.KeyID):
		return errors.New("credential store: credential metadata keyId must be the key's exact opaque id")
	case !boundedText(metadata.Title, 200):
		return errors.New("credential store: credential metadata title is required and at most 200 characters")
	case !fundingAccountPattern.MatchString(metadata.FundingAccount.ID):
		return errors.New("credential store: fundingAccount.id must be kfa_ and 32 lowercase hex digits")
	case !boundedText(metadata.FundingAccount.Label, 320):
		return errors.New("credential store: fundingAccount.label is required and at most 320 characters")
	}
	switch metadata.CapacityCategory {
	case CapacityTrial, CapacityPromotional, CapacityPrepaid, CapacityPaid:
	default:
		return fmt.Errorf("credential store: capacityCategory %q is not trial, promotional, prepaid or paid", metadata.CapacityCategory)
	}
	switch metadata.Environment {
	case "production", "staging", "development":
	default:
		return fmt.Errorf("credential store: environment %q is not production, staging or development", metadata.Environment)
	}
	switch metadata.CommercialUse.Eligibility {
	case CommercialUsePermitted, CommercialUseNotPermitted:
		if !boundedText(metadata.CommercialUse.Evidence, 2000) {
			return errors.New("credential store: a stated commercial-use eligibility needs the evidence it rests on")
		}
	case CommercialUseUnknown:
		if metadata.CommercialUse.Evidence != "" {
			return errors.New("credential store: an unknown commercial-use eligibility carries no evidence")
		}
	default:
		return fmt.Errorf("credential store: commercialUse.eligibility %q is not permitted, not_permitted or unknown", metadata.CommercialUse.Eligibility)
	}
	if restrictions := metadata.Restrictions; restrictions != nil {
		if restrictions.Models == nil && restrictions.Capabilities == nil {
			return errors.New("credential store: restrictions names neither models nor capabilities; omit it for an unrestricted key")
		}
		if err := validateRestrictionList("models", restrictions.Models, 256, func(value string) bool {
			return contract.ModelReference(value).Valid()
		}); err != nil {
			return err
		}
		if err := validateRestrictionList("capabilities", restrictions.Capabilities, 64, capabilityPattern.MatchString); err != nil {
			return err
		}
	}
	return nil
}

func validateRestrictionList(name string, values []string, limit int, valid func(string) bool) error {
	if values == nil {
		return nil
	}
	if len(values) == 0 || len(values) > limit {
		return fmt.Errorf("credential store: restrictions.%s must name between 1 and %d entries", name, limit)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !valid(value) {
			return fmt.Errorf("credential store: restrictions.%s entry %q is invalid", name, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("credential store: restrictions.%s names %q twice", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func boundedText(value string, limit int) bool {
	return value != "" && strings.TrimSpace(value) == value && len([]rune(value)) <= limit
}

// CapacityEvidenceSchemaVersion is the only accepted evidence document shape.
const CapacityEvidenceSchemaVersion = 1

// CapacityEvidence is one observed fact about a key's remaining capacity: a
// quota, a balance or an expiry. Each is exactly one kind, carries where it was
// observed and until when it may be believed, and is recorded append-only.
type CapacityEvidence struct {
	SchemaVersion int                   `json:"schemaVersion"`
	EvidenceID    string                `json:"evidenceId"`
	Provider      contract.ProviderSlug `json:"provider"`
	KeyID         string                `json:"keyId"`
	Kind          string                `json:"kind"`
	Source        string                `json:"source"`
	ObservedAt    time.Time             `json:"observedAt"`
	FreshUntil    *time.Time            `json:"freshUntil,omitempty"`
	Quota         *QuotaEvidence        `json:"quota,omitempty"`
	Balance       *BalanceEvidence      `json:"balance,omitempty"`
	ExpiresAt     *time.Time            `json:"expiresAt,omitempty"`
	Note          string                `json:"note,omitempty"`
}

// QuotaEvidence is a counted allowance in the provider's own unit and window.
type QuotaEvidence struct {
	Unit      string `json:"unit"`
	Window    string `json:"window"`
	Limit     *int64 `json:"limit,omitempty"`
	Remaining int64  `json:"remaining"`
}

// BalanceEvidence is a remaining provider balance, as a decimal string so it
// never passes through floating point.
type BalanceEvidence struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
}

// ParseCapacityEvidence decodes and validates one evidence document.
func ParseCapacityEvidence(raw []byte) (CapacityEvidence, error) {
	var evidence CapacityEvidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return CapacityEvidence{}, fmt.Errorf("credential store: capacity evidence is not a valid document: %w", err)
	}
	if decoder.More() {
		return CapacityEvidence{}, errors.New("credential store: capacity evidence has trailing content")
	}
	if _, err := evidence.balancePicos(); err != nil {
		return CapacityEvidence{}, err
	}
	return evidence, nil
}

func (e CapacityEvidence) balancePicos() (*providercost.Money, error) {
	switch {
	case e.SchemaVersion != CapacityEvidenceSchemaVersion:
		return nil, fmt.Errorf("credential store: capacity evidence schemaVersion must be %d", CapacityEvidenceSchemaVersion)
	case !capacityEvidencePattern.MatchString(e.EvidenceID):
		return nil, errors.New("credential store: evidenceId must be kce_ and 32 lowercase hex digits")
	case !e.Provider.Valid():
		return nil, errors.New("credential store: capacity evidence provider is invalid")
	case !keyIDPattern.MatchString(e.KeyID):
		return nil, errors.New("credential store: capacity evidence keyId must be the key's exact opaque id")
	case e.ObservedAt.IsZero():
		return nil, errors.New("credential store: capacity evidence needs observedAt")
	case e.FreshUntil != nil && !e.FreshUntil.After(e.ObservedAt):
		return nil, errors.New("credential store: freshUntil must follow observedAt")
	case e.Note != "" && !boundedText(e.Note, 2000):
		return nil, errors.New("credential store: note is at most 2000 characters without surrounding space")
	}
	switch e.Source {
	case "provider_api", "provider_console", "provider_email", "operator":
	default:
		return nil, fmt.Errorf("credential store: capacity evidence source %q is invalid", e.Source)
	}
	present := 0
	for _, set := range []bool{e.Quota != nil, e.Balance != nil, e.ExpiresAt != nil} {
		if set {
			present++
		}
	}
	if present != 1 {
		return nil, errors.New("credential store: capacity evidence carries exactly one of quota, balance or expiresAt")
	}
	switch e.Kind {
	case "quota":
		quota := e.Quota
		if quota == nil || !quotaUnitPattern.MatchString(quota.Unit) || quota.Remaining < 0 ||
			(quota.Limit != nil && (*quota.Limit < 0 || quota.Remaining > *quota.Limit)) ||
			!slices.Contains([]string{"minute", "hour", "day", "month", "lifetime"}, quota.Window) {
			return nil, errors.New("credential store: quota evidence needs a unit, a window and a remaining count within its limit")
		}
		return nil, nil
	case "balance":
		if e.Balance == nil {
			return nil, errors.New("credential store: balance evidence needs a balance")
		}
		money, err := providercost.ParseDecimal(e.Balance.Currency, e.Balance.Amount)
		if err != nil {
			return nil, fmt.Errorf("credential store: balance evidence: %w", err)
		}
		return &money, nil
	case "expiry":
		if e.ExpiresAt == nil || e.ExpiresAt.IsZero() {
			return nil, errors.New("credential store: expiry evidence needs expiresAt")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("credential store: capacity evidence kind %q is not quota, balance or expiry", e.Kind)
	}
}

// PutCredentialMetadata applies one metadata document for an exact key. It
// reports "applied" or "replayed"; a reused operation id with a different
// document, or a funding account id reused with a different label, is refused.
func (p *Postgres) PutCredentialMetadata(ctx context.Context, metadata CredentialMetadata, actor string) (string, error) {
	if err := ValidateCredentialMetadata(metadata); err != nil {
		return "", err
	}
	if err := validateActor(actor); err != nil {
		return "", err
	}
	document, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("credential store: encoding credential metadata: %w", err)
	}
	var outcome string
	if err := p.pool.QueryRow(ctx, `SELECT kaana_put_provider_credential_description($1, $2::jsonb, $3)`,
		metadata.OperationID, document, actor).Scan(&outcome); err != nil {
		return "", fmt.Errorf("credential store: putting credential metadata %s: %w", metadata.OperationID, err)
	}
	return outcome, nil
}

// RecordCapacityEvidence appends one capacity observation for an exact key.
func (p *Postgres) RecordCapacityEvidence(ctx context.Context, evidence CapacityEvidence, actor string) (string, error) {
	balance, err := evidence.balancePicos()
	if err != nil {
		return "", err
	}
	if err := validateActor(actor); err != nil {
		return "", err
	}
	type balanceJSON struct {
		Currency    string `json:"currency"`
		AmountPicos int64  `json:"amountPicos"`
	}
	stored := struct {
		CapacityEvidence
		Balance *balanceJSON `json:"balance,omitempty"`
	}{CapacityEvidence: evidence}
	if balance != nil {
		stored.Balance = &balanceJSON{Currency: balance.Currency, AmountPicos: balance.Amount}
	}
	document, err := json.Marshal(stored)
	if err != nil {
		return "", fmt.Errorf("credential store: encoding capacity evidence: %w", err)
	}
	var outcome string
	if err := p.pool.QueryRow(ctx, `SELECT kaana_record_provider_credential_capacity_evidence($1::jsonb, $2)`,
		document, actor).Scan(&outcome); err != nil {
		return "", fmt.Errorf("credential store: recording capacity evidence %s: %w", evidence.EvidenceID, err)
	}
	return outcome, nil
}

// CredentialMetadataListing is the operator projection of one key's metadata,
// with its latest observation of each capacity kind.
type CredentialMetadataListing struct {
	Provider               contract.ProviderSlug `json:"provider"`
	KeyID                  string                `json:"keyId"`
	Revision               int                   `json:"revision"`
	Title                  string                `json:"title"`
	FundingAccountID       string                `json:"fundingAccountId"`
	FundingAccountLabel    string                `json:"fundingAccountLabel"`
	SharedWithKeys         []string              `json:"sharedWithKeys"`
	CapacityCategory       CapacityCategory      `json:"capacityCategory"`
	Environment            string                `json:"environment"`
	CommercialUse          CommercialUse         `json:"commercialUse"`
	CommercialUseEvidence  string                `json:"commercialUseEvidence,omitempty"`
	AllowedModels          []string              `json:"allowedModels,omitempty"`
	AllowedCapabilities    []string              `json:"allowedCapabilities,omitempty"`
	UpdatedAt              time.Time             `json:"updatedAt"`
	LatestCapacityEvidence []CapacityObservation `json:"latestCapacityEvidence"`
}

// CapacityObservation is the most recent evidence of one kind, with whether it
// is still fresh at listing time. Evidence without a freshUntil is never fresh.
type CapacityObservation struct {
	EvidenceID     string     `json:"evidenceId"`
	Kind           string     `json:"kind"`
	Source         string     `json:"source"`
	ObservedAt     time.Time  `json:"observedAt"`
	FreshUntil     *time.Time `json:"freshUntil,omitempty"`
	Fresh          bool       `json:"fresh"`
	QuotaUnit      *string    `json:"quotaUnit,omitempty"`
	QuotaWindow    *string    `json:"quotaWindow,omitempty"`
	QuotaLimit     *int64     `json:"quotaLimit,omitempty"`
	QuotaRemaining *int64     `json:"quotaRemaining,omitempty"`
	Balance        *string    `json:"balance,omitempty"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
}

// ListCredentialMetadata returns every described key for an operator.
func (p *Postgres) ListCredentialMetadata(ctx context.Context, at time.Time) ([]CredentialMetadataListing, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT m.provider_slug, m.key_id, m.revision, m.title, m.funding_account_id, a.account_label,
		       COALESCE((SELECT array_agg(other.key_id ORDER BY other.key_id)
		                   FROM provider_credential_descriptions other
		                  WHERE other.funding_account_id = m.funding_account_id
		                    AND (other.provider_slug, other.key_id) <> (m.provider_slug, m.key_id)), ARRAY[]::TEXT[]),
		       m.capacity_category, m.environment, m.commercial_use, COALESCE(m.commercial_use_evidence, ''),
		       m.allowed_models, m.allowed_capabilities, m.updated_at
		  FROM provider_credential_descriptions m
		  JOIN provider_funding_accounts a ON a.funding_account_id = m.funding_account_id
		 ORDER BY m.provider_slug, m.key_id`)
	if err != nil {
		return nil, fmt.Errorf("credential store: listing credential metadata: %w", err)
	}
	listings := make([]CredentialMetadataListing, 0)
	for rows.Next() {
		var row CredentialMetadataListing
		if err := rows.Scan(&row.Provider, &row.KeyID, &row.Revision, &row.Title, &row.FundingAccountID,
			&row.FundingAccountLabel, &row.SharedWithKeys, &row.CapacityCategory, &row.Environment,
			&row.CommercialUse, &row.CommercialUseEvidence, &row.AllowedModels, &row.AllowedCapabilities,
			&row.UpdatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("credential store: reading credential metadata: %w", err)
		}
		row.LatestCapacityEvidence = make([]CapacityObservation, 0)
		listings = append(listings, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading credential metadata: %w", err)
	}

	evidenceRows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (provider_slug, key_id, kind)
		       provider_slug, key_id, evidence_id, kind, source, observed_at, fresh_until,
		       quota_unit, quota_window, quota_limit::BIGINT, quota_remaining::BIGINT,
		       balance_currency, balance_picos::BIGINT, expires_at
		  FROM provider_credential_capacity_evidence
		 ORDER BY provider_slug, key_id, kind, observed_at DESC, recorded_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("credential store: listing capacity evidence: %w", err)
	}
	defer evidenceRows.Close()
	index := make(map[string]int, len(listings))
	for position, listing := range listings {
		index[string(listing.Provider)+"\x00"+listing.KeyID] = position
	}
	for evidenceRows.Next() {
		var (
			providerSlug, keyID string
			observation         CapacityObservation
			currency            *string
			picos               *int64
		)
		if err := evidenceRows.Scan(&providerSlug, &keyID, &observation.EvidenceID, &observation.Kind,
			&observation.Source, &observation.ObservedAt, &observation.FreshUntil, &observation.QuotaUnit,
			&observation.QuotaWindow, &observation.QuotaLimit, &observation.QuotaRemaining, &currency, &picos,
			&observation.ExpiresAt); err != nil {
			return nil, fmt.Errorf("credential store: reading capacity evidence: %w", err)
		}
		observation.Fresh = observation.FreshUntil != nil && at.Before(*observation.FreshUntil)
		if currency != nil && picos != nil {
			rendered := providercost.Money{Currency: *currency, Amount: *picos}.String()
			observation.Balance = &rendered
		}
		position, described := index[providerSlug+"\x00"+keyID]
		if !described {
			// Evidence for a key nobody has described yet is still evidence; it
			// is listed once the key has metadata rather than dropped.
			continue
		}
		listings[position].LatestCapacityEvidence = append(listings[position].LatestCapacityEvidence, observation)
	}
	if err := evidenceRows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading capacity evidence: %w", err)
	}
	return listings, nil
}
