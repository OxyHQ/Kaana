package credentialstore

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0001_provider_credentials.sql
var migration0001 string

//go:embed migrations/0002_database_privileges.sql
var migration0002 string

//go:embed migrations/0003_customer_provider_credentials.sql
var migration0003 string

//go:embed migrations/0004_customer_credential_operation_outcomes.sql
var migration0004 string

//go:embed migrations/0005_customer_credential_outcome_without_digest.sql
var migration0005 string

//go:embed migrations/0006_customer_credential_validations.sql
var migration0006 string

//go:embed migrations/0007_provider_credential_id_operations.sql
var migration0007 string

//go:embed migrations/0008_provider_cost_events.sql
var migration0008 string

//go:embed migrations/0009_platform_provider_credential_operations.sql
var migration0009 string

//go:embed migrations/0011_provider_credential_runtime.sql
var migration0011 string

//go:embed migrations/0012_runtime_state_read_authority.sql
var migration0012 string

//go:embed migrations/0013_deployment_credential_bindings.sql
var migration0013 string

//go:embed migrations/0010_provider_cost_event_batches.sql
var migration0010 string

// Postgres owns a bounded connection pool to Kaana's database.
type Postgres struct {
	pool             *pgxpool.Pool
	databaseIdentity postgresDatabaseIdentity
}

type postgresDatabaseIdentity struct {
	host     string
	port     uint16
	database string
}

// OpenPostgres connects and proves the database is reachable.
func OpenPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	if databaseURL == "" {
		return nil, errors.New("credential store: DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx parse failures may quote the input. DATABASE_URL contains the
		// database password, so the original error must not become a log line.
		return nil, errors.New("credential store: DATABASE_URL is invalid")
	}
	if err := requireVerifiedPostgresTLS(config); err != nil {
		return nil, err
	}
	config.MaxConns = 4
	config.MinConns = 0
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("credential store: opening PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("credential store: pinging PostgreSQL: %w", err)
	}
	return &Postgres{
		pool: pool,
		databaseIdentity: postgresDatabaseIdentity{
			host: config.ConnConfig.Host, port: config.ConnConfig.Port, database: config.ConnConfig.Database,
		},
	}, nil
}

// requireVerifiedPostgresTLS refuses encryption without server identity
// verification. sslmode=require prevents passive reading, but still lets an
// active attacker impersonate PostgreSQL and collect the database credential.
func requireVerifiedPostgresTLS(config *pgxpool.Config) error {
	verified := func(serverName string, insecureSkipVerify bool) bool {
		return serverName != "" && !insecureSkipVerify
	}
	if config.ConnConfig.TLSConfig == nil || !verified(
		config.ConnConfig.TLSConfig.ServerName,
		config.ConnConfig.TLSConfig.InsecureSkipVerify,
	) {
		return errors.New("credential store: DATABASE_URL must use sslmode=verify-full with a trusted root")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if fallback.TLSConfig == nil || !verified(fallback.TLSConfig.ServerName, fallback.TLSConfig.InsecureSkipVerify) {
			return errors.New("credential store: DATABASE_URL contains an unverified PostgreSQL fallback")
		}
	}
	return nil
}

// Close releases the pool.
func (p *Postgres) Close() { p.pool.Close() }

// Migrate applies schema and grants with the dedicated migrator identity. Role
// creation and password rotation are a separate master-authority one-shot.
func (p *Postgres) Migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("credential store: beginning migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := migratePostgres(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("credential store: committing migrations: %w", err)
	}
	return nil
}

type migrationExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func migratePostgres(ctx context.Context, tx migrationExecutor) error {
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS kaana_schema_migrations (
			version TEXT PRIMARY KEY,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("credential store: creating migration ledger: %w", err)
	}

	for _, migration := range []struct {
		version string
		body    string
	}{
		{version: "0001", body: migration0001},
		{version: "0002", body: migration0002},
		{version: "0003", body: migration0003},
		{version: "0004", body: migration0004},
		{version: "0005", body: migration0005},
		{version: "0006", body: migration0006},
		{version: "0007", body: migration0007},
		{version: "0008", body: migration0008},
		{version: "0009", body: migration0009},
		{version: "0010", body: migration0010},
		{version: "0011", body: migration0011},
		{version: "0012", body: migration0012},
		{version: "0013", body: migration0013},
	} {
		if err := applyMigration(ctx, tx, migration.version, migration.body); err != nil {
			return err
		}
	}
	return nil
}

// WriteProviderCostEvent uses the same atomic batch boundary for a single
// attempt. The runtime role has no execute grant on the retired row-at-a-time
// function after migration 0010.
func (p *Postgres) WriteProviderCostEvent(ctx context.Context, event providercost.Event) error {
	return p.WriteProviderCostEvents(ctx, []providercost.Event{event})
}

type providerCostEventJSON struct {
	RequestID         contract.RequestID      `json:"request_id"`
	AttemptIndex      int                     `json:"attempt_index"`
	Provider          contract.ProviderSlug   `json:"provider_slug"`
	KeyID             string                  `json:"key_id"`
	DeploymentID      contract.DeploymentID   `json:"deployment_id"`
	ModelReference    contract.ModelReference `json:"model_reference"`
	Currency          *string                 `json:"currency"`
	AmountPicos       *int64                  `json:"amount_picos"`
	Source            providercost.Source     `json:"source"`
	RateCardVersionID string                  `json:"rate_card_version_id,omitempty"`
	Complete          bool                    `json:"complete"`
	Served            bool                    `json:"served"`
	OccurredAt        time.Time               `json:"occurred_at"`
}

// WriteProviderCostEvents persists every platform-funded attempt for one
// request through one PostgreSQL statement and transaction. An error commits
// none of the attempts, so Recorder may safely retry the whole batch.
func (p *Postgres) WriteProviderCostEvents(ctx context.Context, events []providercost.Event) error {
	encodedEvents := make([]providerCostEventJSON, 0, len(events))
	for _, event := range events {
		encoded := providerCostEventJSON{
			RequestID: event.RequestID, AttemptIndex: event.AttemptIndex,
			Provider: event.Provider, KeyID: event.KeyID,
			DeploymentID: event.DeploymentID, ModelReference: event.ModelReference,
			Source: event.Source, RateCardVersionID: event.RateCardVersionID,
			Complete: event.Complete, Served: event.Served,
			OccurredAt: event.OccurredAt,
		}
		if event.Source != providercost.SourceUnknown {
			encoded.Currency = &event.Cost.Currency
			encoded.AmountPicos = &event.Cost.Amount
		}
		encodedEvents = append(encodedEvents, encoded)
	}
	payload, err := json.Marshal(encodedEvents)
	if err != nil {
		return fmt.Errorf("credential store: encoding provider cost event batch: %w", err)
	}
	var recorded int
	if err := p.pool.QueryRow(ctx, `SELECT kaana_record_provider_cost_events($1::jsonb)`, payload).Scan(&recorded); err != nil {
		return fmt.Errorf("credential store: recording provider cost event batch: %w", err)
	}
	if recorded != len(events) {
		return fmt.Errorf("credential store: provider cost event batch recorded %d of %d attempts", recorded, len(events))
	}
	return nil
}

func applyMigration(ctx context.Context, tx migrationExecutor, version, body string) error {
	checksum := migrationChecksum(body)
	var appliedChecksum string
	err := tx.QueryRow(ctx, `SELECT checksum FROM kaana_schema_migrations WHERE version = $1`, version).Scan(&appliedChecksum)
	switch {
	case err == nil:
		if appliedChecksum != checksum {
			return fmt.Errorf("credential store: applied migration %s checksum differs from this binary", version)
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("credential store: reading migration %s ledger: %w", version, err)
	}

	if version == "0001" {
		var unmanaged bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('public.provider_credentials') IS NOT NULL OR to_regclass('public.provider_credential_audit') IS NOT NULL`).Scan(&unmanaged); err != nil {
			return fmt.Errorf("credential store: inspecting schema before migration: %w", err)
		}
		if unmanaged {
			return errors.New("credential store: credential tables exist without migration ledger entry 0001")
		}
	}

	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("credential store: applying migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kaana_schema_migrations (version, checksum) VALUES ($1, $2)`, version, checksum); err != nil {
		return fmt.Errorf("credential store: recording migration %s: %w", version, err)
	}
	return nil
}

func migrationChecksum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// ListEnabled returns ciphertext ordered exactly as the provider pool spends
// it. Provider filters are parameterized; no identifier is interpolated.
func (p *Postgres) ListEnabled(ctx context.Context, providers []contract.ProviderSlug) ([]EncryptedCredential, error) {
	names := make([]string, 0, len(providers))
	for _, slug := range providers {
		names = append(names, string(slug))
	}
	rows, err := p.pool.Query(ctx, `
		SELECT provider_slug, key_id, encrypted_secret, kms_key_arn,
		       key_class, budget_usd::double precision, position,
		       runtime.state, runtime.evidence_source, runtime.observed_at, runtime.retired_until, runtime.lease_until
		FROM active_provider_credentials AS credential
		LEFT JOIN provider_credential_runtime_state AS runtime USING (provider_slug, key_id)
		WHERE provider_slug = ANY($1::text[])
		ORDER BY provider_slug, position, key_id`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	credentials := make([]EncryptedCredential, 0)
	for rows.Next() {
		var (
			row          EncryptedCredential
			providerSlug string
			class        string
			budget       pgtype.Float8
			state        pgtype.Text
			evidence     pgtype.Text
			observedAt   pgtype.Timestamptz
			retiredUntil pgtype.Timestamptz
			leaseUntil   pgtype.Timestamptz
		)
		if err := rows.Scan(&providerSlug, &row.KeyID, &row.Ciphertext, &row.KMSKeyARN, &class, &budget, &row.Position, &state, &evidence, &observedAt, &retiredUntil, &leaseUntil); err != nil {
			return nil, err
		}
		row.Provider = contract.ProviderSlug(providerSlug)
		row.Class = provider.KeyClass(class)
		if budget.Valid {
			value := budget.Float64
			row.BudgetUSD = &value
		}
		if state.Valid {
			if state.String != "usable" {
				row.Runtime.Reason = provider.KeyRetirement(state.String)
			}
		}
		if evidence.Valid {
			row.Runtime.Evidence = evidence.String
		}
		if observedAt.Valid {
			row.Runtime.ObservedAt = observedAt.Time
		}
		if retiredUntil.Valid {
			row.Runtime.RetiredUntil = retiredUntil.Time
		}
		if leaseUntil.Valid {
			row.Runtime.LeaseUntil = leaseUntil.Time
		}
		credentials = append(credentials, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return credentials, nil
}

func (p *Postgres) ClaimCredentialRecovery(ctx context.Context, slug contract.ProviderSlug, keyID string, at, until time.Time) (provider.CredentialRecoveryDecision, error) {
	var decision string
	err := p.pool.QueryRow(ctx, `SELECT kaana_claim_provider_credential_recovery($1,$2,$3,$4)`, slug, keyID, at, until).Scan(&decision)
	return provider.CredentialRecoveryDecision(decision), err
}

func (p *Postgres) RecordCredentialAttempt(ctx context.Context, attempt provider.CredentialAttempt) error {
	var retired any
	if !attempt.RetiredUntil.IsZero() {
		retired = attempt.RetiredUntil
	}
	_, err := p.pool.Exec(ctx, `SELECT kaana_record_provider_credential_attempt($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		attempt.RequestID, attempt.DeploymentID, attempt.Index, attempt.Provider, attempt.KeyID,
		attempt.Outcome, attempt.Evidence, attempt.OccurredAt, retired)
	return err
}

// Put atomically creates or rotates a named credential through the only
// database function the credential-admin role may execute. The role has no
// direct table DML, so the credential row and its audit row cannot diverge.
func (p *Postgres) Put(ctx context.Context, row EncryptedCredential, actor string) error {
	_, err := p.pool.Exec(ctx, `SELECT kaana_put_provider_credential($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.Provider, row.KeyID, row.Ciphertext, row.KMSKeyARN,
		row.Class, row.BudgetUSD, row.Position, actor)
	return err
}

// Disable is idempotent and reports whether it changed an active row.
func (p *Postgres) Disable(ctx context.Context, scope Scope, actor string) (bool, error) {
	var changed bool
	err := p.pool.QueryRow(ctx, `SELECT kaana_disable_provider_credential($1, $2, $3)`, scope.Provider, scope.KeyID, actor).Scan(&changed)
	return changed, err
}

// ListMetadata deliberately does not select encrypted_secret.
func (p *Postgres) ListMetadata(ctx context.Context) ([]Metadata, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT provider_slug, key_id, kms_key_arn, key_class,
		       budget_usd::double precision, position, enabled
		FROM provider_credential_metadata
		ORDER BY provider_slug, position, key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	metadata := make([]Metadata, 0)
	for rows.Next() {
		var (
			row          Metadata
			providerSlug string
			class        string
			budget       pgtype.Float8
		)
		if err := rows.Scan(&providerSlug, &row.KeyID, &row.KMSKeyARN, &class, &budget, &row.Position, &row.Enabled); err != nil {
			return nil, err
		}
		row.Provider = contract.ProviderSlug(providerSlug)
		row.Class = provider.KeyClass(class)
		if budget.Valid {
			value := budget.Float64
			row.BudgetUSD = &value
		}
		metadata = append(metadata, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return metadata, nil
}
