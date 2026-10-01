package credentialstore

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func reviewedMigrationFixture(t *testing.T) (*Postgres, context.Context) {
	t.Helper()
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("reviewed_migration_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, cleanupErr := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); cleanupErr != nil {
			t.Error(cleanupErr)
		}
		admin.Close()
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `CREATE TABLE kaana_schema_migrations(version TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range schemaMigrations() {
		if migration.version == "0020" {
			break
		}
		if err = applyMigration(ctx, tx, migration.version, migration.body); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return &Postgres{pool: pool}, ctx
}
func TestReviewedScopedMigrationInspectionIsReadOnlyAndApplyPreservesAuthority(t *testing.T) {
	repo, ctx := reviewedMigrationFixture(t)
	report, err := repo.ReviewedScopedMigration(ctx, true)
	if err != nil || !report.ReadOnly || !reflect.DeepEqual(report.Pending, []string{"0020"}) || report.Checksum != migrationChecksum(migration0020) {
		t.Fatal(report, err)
	}
	var absent bool
	if err = repo.pool.QueryRow(ctx, `SELECT to_regclass('public.scoped_provider_attempt_claims') IS NULL AND NOT EXISTS(SELECT 1 FROM kaana_schema_migrations WHERE version='0020')`).Scan(&absent); err != nil || !absent {
		t.Fatal("inspection performed DDL", err)
	}
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := reviewedSchemaAuthority(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	applied, err := repo.ReviewedScopedMigration(ctx, false)
	if err != nil || applied.ReadOnly || !applied.RecorderAuthorityUnchanged {
		t.Fatal(applied, err)
	}
	tx, err = repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reviewedSchemaAuthority(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err = reviewedClaimTablePrivate(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("existing relation/function ACL, owner or membership changed")
	}
	inspected, err := repo.ReviewedScopedMigration(ctx, true)
	if err != nil || len(inspected.Pending) != 0 || !inspected.ReadOnly {
		t.Fatal(inspected, err)
	}
	again, err := repo.ReviewedScopedMigration(ctx, false)
	if err != nil || len(again.Pending) != 0 {
		t.Fatal("repeat changed schema", again, err)
	}
}
func TestReviewedScopedMigrationRejectsUnreviewedStateBeforeDDL(t *testing.T) {
	cases := map[string]string{
		"missing older migration": `DELETE FROM kaana_schema_migrations WHERE version='0019'`,
		"checksum mismatch":       `UPDATE kaana_schema_migrations SET checksum='wrong' WHERE version='0019'`,
		"unknown ledger entry":    `INSERT INTO kaana_schema_migrations(version,checksum) VALUES('0021','unknown')`,
		"unmanaged claim table":   `CREATE TABLE scoped_provider_attempt_claims(fake INTEGER)`,
		"recorder search path":    `ALTER FUNCTION kaana_record_provider_credential_attempt(text,text,integer,text,text,text,text,timestamp with time zone,timestamp with time zone) SET search_path=public`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			repo, ctx := reviewedMigrationFixture(t)
			if _, err := repo.pool.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			for _, inspect := range []bool{true, false} {
				if _, err := repo.ReviewedScopedMigration(ctx, inspect); err == nil {
					t.Fatal("unreviewed state admitted")
				}
			}
			var applied bool
			if err := repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM kaana_schema_migrations WHERE version='0020')`).Scan(&applied); err != nil || applied {
				t.Fatal("unexpected mutation", err)
			}
		})
	}
}
func TestReviewedScopedMigrationRollsBackDefaultGrantExpansion(t *testing.T) {
	repo, ctx := reviewedMigrationFixture(t)
	if _, err := repo.pool.Exec(ctx, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO kaana_runtime`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReviewedScopedMigration(ctx, false); err == nil {
		t.Fatal("default grant widened claim-table visibility")
	}
	var absent bool
	if err := repo.pool.QueryRow(ctx, `SELECT to_regclass('public.scoped_provider_attempt_claims') IS NULL AND NOT EXISTS(SELECT 1 FROM kaana_schema_migrations WHERE version='0020')`).Scan(&absent); err != nil || !absent {
		t.Fatal("failed authority check did not roll back DDL", err)
	}
}
