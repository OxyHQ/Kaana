package credentialstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// PublicationEvidence is the non-secret state the inventory publisher reads
// to decide whether a discovered deployment can be served now
// (docs/inventory.md, "Withheld from publication"). It carries no ciphertext,
// no label and no evidence text: identities, states and times.
type PublicationEvidence struct {
	Keys     []PublicationKey
	Bindings []provider.CredentialBinding
	Failures []DeploymentFailureStreak
}

// PublicationKey is one enabled platform key's persisted runtime state and
// its latest capacity evidence of each kind.
type PublicationKey struct {
	Provider contract.ProviderSlug
	KeyID    string
	Runtime  provider.CredentialRuntimeState
	Capacity []PublicationCapacity
}

// PublicationCapacity is one migration-0017 observation as the economics
// read projects it. Amounts stay decimal strings: only zero matters here.
type PublicationCapacity struct {
	Kind           string
	ObservedAt     time.Time
	FreshUntil     time.Time
	QuotaWindow    string
	QuotaRemaining string
	BalancePicos   string
	ExpiresAt      time.Time
}

// Empty reports a quota or balance that reads exactly zero.
func (c PublicationCapacity) Empty() bool {
	switch c.Kind {
	case "quota":
		return c.QuotaRemaining == "0"
	case "balance":
		return c.BalancePicos == "0"
	default:
		return false
	}
}

// DeploymentFailureStreak is one failure code's share of the failed attempts
// a deployment made on one key since its last success there (migration 0019).
type DeploymentFailureStreak struct {
	DeploymentID contract.DeploymentID
	Provider     contract.ProviderSlug
	KeyID        string
	FailureCode  contract.ErrorCode
	Failures     int
	FirstAt      time.Time
	LastAt       time.Time
}

// ReadPublicationEvidence reads everything in one read-only snapshot, so a key
// state, a binding and a streak can never come from two different moments.
func (p *Postgres) ReadPublicationEvidence(ctx context.Context, providers []contract.ProviderSlug, since time.Time) (PublicationEvidence, error) {
	if len(providers) == 0 {
		return PublicationEvidence{}, errors.New("credential store: publication evidence needs at least one provider")
	}
	names := make([]string, 0, len(providers))
	wanted := make(map[contract.ProviderSlug]struct{}, len(providers))
	for _, slug := range providers {
		names = append(names, string(slug))
		wanted[slug] = struct{}{}
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PublicationEvidence{}, fmt.Errorf("credential store: beginning the publication evidence read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var evidence PublicationEvidence
	if evidence.Keys, err = readPublicationKeys(ctx, tx, names); err != nil {
		return PublicationEvidence{}, err
	}
	if err := attachCapacityEvidence(ctx, tx, evidence.Keys); err != nil {
		return PublicationEvidence{}, err
	}
	if evidence.Bindings, err = readPublicationBindings(ctx, tx, names); err != nil {
		return PublicationEvidence{}, err
	}
	if evidence.Failures, err = readFailureStreaks(ctx, tx, since, wanted); err != nil {
		return PublicationEvidence{}, err
	}
	return evidence, nil
}

func readPublicationKeys(ctx context.Context, tx pgx.Tx, providers []string) ([]PublicationKey, error) {
	rows, err := tx.Query(ctx, `
		SELECT credential.provider_slug, credential.key_id,
		       runtime.state, runtime.evidence_source, runtime.observed_at, runtime.retired_until, runtime.lease_until
		FROM active_provider_credentials AS credential
		LEFT JOIN provider_credential_runtime_state AS runtime USING (provider_slug, key_id)
		WHERE credential.provider_slug = ANY($1::text[])
		ORDER BY credential.provider_slug, credential.position, credential.key_id`, providers)
	if err != nil {
		return nil, fmt.Errorf("credential store: listing publication key state: %w", err)
	}
	defer rows.Close()
	keys := make([]PublicationKey, 0)
	for rows.Next() {
		var (
			key                                  PublicationKey
			slug                                 string
			state, source                        pgtype.Text
			observedAt, retiredUntil, leaseUntil pgtype.Timestamptz
		)
		if err := rows.Scan(&slug, &key.KeyID, &state, &source, &observedAt, &retiredUntil, &leaseUntil); err != nil {
			return nil, fmt.Errorf("credential store: reading publication key state: %w", err)
		}
		key.Provider = contract.ProviderSlug(slug)
		if state.Valid && state.String != "usable" {
			key.Runtime.Reason = provider.KeyRetirement(state.String)
		}
		key.Runtime.Evidence = source.String
		key.Runtime.ObservedAt = observedAt.Time
		key.Runtime.RetiredUntil = retiredUntil.Time
		key.Runtime.LeaseUntil = leaseUntil.Time
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading publication key state: %w", err)
	}
	return keys, nil
}

// capacityEvidenceJSON is one element of the economics read's
// capacity_evidence array (migration 0018).
type capacityEvidenceJSON struct {
	Kind           string      `json:"kind"`
	ObservedAt     time.Time   `json:"observedAt"`
	FreshUntil     *time.Time  `json:"freshUntil"`
	QuotaWindow    string      `json:"quotaWindow"`
	QuotaRemaining json.Number `json:"quotaRemaining"`
	BalancePicos   string      `json:"balancePicos"`
	ExpiresAt      *time.Time  `json:"expiresAt"`
}

// attachCapacityEvidence reads through the economics function the runtime is
// already granted, rather than a second grant on the evidence table.
func attachCapacityEvidence(ctx context.Context, tx pgx.Tx, keys []PublicationKey) error {
	index := make(map[Scope]int, len(keys))
	for position, key := range keys {
		index[Scope{Provider: key.Provider, KeyID: key.KeyID}] = position
	}
	rows, err := tx.Query(ctx, `SELECT provider_slug, key_id, capacity_evidence FROM kaana_read_provider_credential_economics()`)
	if err != nil {
		return fmt.Errorf("credential store: reading capacity evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			slug, keyID string
			raw         []byte
		)
		if err := rows.Scan(&slug, &keyID, &raw); err != nil {
			return fmt.Errorf("credential store: reading capacity evidence: %w", err)
		}
		position, wanted := index[Scope{Provider: contract.ProviderSlug(slug), KeyID: keyID}]
		if !wanted {
			continue
		}
		var observations []capacityEvidenceJSON
		if err := json.Unmarshal(raw, &observations); err != nil {
			return fmt.Errorf("credential store: provider %q key %q capacity evidence is unreadable: %w", slug, keyID, err)
		}
		for _, observation := range observations {
			converted := PublicationCapacity{
				Kind: observation.Kind, ObservedAt: observation.ObservedAt,
				QuotaWindow: observation.QuotaWindow, QuotaRemaining: observation.QuotaRemaining.String(),
				BalancePicos: observation.BalancePicos,
			}
			if observation.FreshUntil != nil {
				converted.FreshUntil = *observation.FreshUntil
			}
			if observation.ExpiresAt != nil {
				converted.ExpiresAt = *observation.ExpiresAt
			}
			keys[position].Capacity = append(keys[position].Capacity, converted)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("credential store: reading capacity evidence: %w", err)
	}
	return nil
}

func readPublicationBindings(ctx context.Context, tx pgx.Tx, providers []string) ([]provider.CredentialBinding, error) {
	rows, err := tx.Query(ctx, `SELECT b.deployment_id, b.provider_slug, b.key_id
		FROM provider_deployment_credential_bindings b
		JOIN active_provider_credentials c ON c.provider_slug = b.provider_slug AND c.key_id = b.key_id
		WHERE b.provider_slug = ANY($1::text[])
		ORDER BY b.deployment_id`, providers)
	if err != nil {
		return nil, fmt.Errorf("credential store: listing publication bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]provider.CredentialBinding, 0)
	for rows.Next() {
		var binding provider.CredentialBinding
		if err := rows.Scan(&binding.DeploymentID, &binding.Provider, &binding.KeyID); err != nil {
			return nil, fmt.Errorf("credential store: reading publication binding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading publication bindings: %w", err)
	}
	return bindings, nil
}

func readFailureStreaks(ctx context.Context, tx pgx.Tx, since time.Time, wanted map[contract.ProviderSlug]struct{}) ([]DeploymentFailureStreak, error) {
	rows, err := tx.Query(ctx, `SELECT deployment_id, provider_slug, key_id, failure_code, failures, first_failed_at, last_failed_at
		FROM kaana_read_deployment_failure_streaks($1)`, since)
	if err != nil {
		return nil, fmt.Errorf("credential store: reading deployment failure streaks: %w", err)
	}
	defer rows.Close()
	streaks := make([]DeploymentFailureStreak, 0)
	for rows.Next() {
		var (
			streak DeploymentFailureStreak
			code   string
		)
		if err := rows.Scan(&streak.DeploymentID, &streak.Provider, &streak.KeyID, &code, &streak.Failures, &streak.FirstAt, &streak.LastAt); err != nil {
			return nil, fmt.Errorf("credential store: reading a deployment failure streak: %w", err)
		}
		if _, asked := wanted[streak.Provider]; !asked {
			continue
		}
		streak.FailureCode = contract.ErrorCode(code)
		streaks = append(streaks, streak)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading deployment failure streaks: %w", err)
	}
	return streaks, nil
}
