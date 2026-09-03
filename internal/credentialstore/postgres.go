package credentialstore

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
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

// Postgres owns a bounded connection pool to Kaana's database.
type Postgres struct {
	pool *pgxpool.Pool
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
	return &Postgres{pool: pool}, nil
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

// Migrate creates the credentials table. It is deliberately an operator
// action: the serving role needs SELECT, not DDL authority.
func (p *Postgres) Migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("credential store: beginning migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
	} {
		if err := applyMigration(ctx, tx, migration.version, migration.body); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("credential store: committing migrations: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, tx pgx.Tx, version, body string) error {
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
		       key_class, budget_usd::double precision, position
		FROM active_provider_credentials
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
		)
		if err := rows.Scan(&providerSlug, &row.KeyID, &row.Ciphertext, &row.KMSKeyARN, &class, &budget, &row.Position); err != nil {
			return nil, err
		}
		row.Provider = contract.ProviderSlug(providerSlug)
		row.Class = provider.KeyClass(class)
		if budget.Valid {
			value := budget.Float64
			row.BudgetUSD = &value
		}
		credentials = append(credentials, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return credentials, nil
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
