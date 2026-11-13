package nativeadmission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every query is observed on the actual PostgreSQL connection. Removing the
// transaction's READ ONLY setting makes this assertion fail, even if a writer
// happened not to change values in this particular fixture.
type inspectionTracer struct {
	sql []string
}

func (tr *inspectionTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.sql = append(tr.sql, data.SQL)
	return ctx
}
func (*inspectionTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresInspectionShowsDisabledThenImportedWithoutWrites(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	config := pool.Config().Copy()
	trace := &inspectionTracer{}
	config.ConnConfig.Tracer = trace
	reader, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	inspect := NewStore(reader)
	report, err := inspect.InspectPolicy(ctx, p)
	if err != nil || report.MigrationState != "verified" || !report.EnrollmentPresent || !report.Registered || report.Enabled || report.ActivePolicy != nil || report.LegacyFenceComplete {
		t.Fatal(report, err)
	}
	before := snapshotInspectionRows(t, pool, ctx)
	raw, review := testLegacyManifest(t, p)
	if _, err = s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	activeBefore := snapshotInspectionRows(t, pool, ctx)
	report, err = inspect.InspectPolicy(ctx, p)
	if err != nil || !report.Enabled || !report.Registered || !report.LegacyFenceComplete || report.ActivePolicy == nil || report.ActivePolicy.Version != p.Version || report.ActivePolicy.Digest != policyDigest(p) {
		t.Fatal(report, err)
	}
	if activeBefore != snapshotInspectionRows(t, pool, ctx) {
		t.Fatal("inspection changed durable state")
	}
	if before == activeBefore {
		t.Fatal("import positive control did not change durable state")
	}
	for _, query := range trace.sql {
		if strings.HasPrefix(query, "begin") {
			if !strings.Contains(query, "isolation level repeatable read read only") {
				t.Fatal("inspection lost read-only snapshot")
			}
		} else if !strings.HasPrefix(query, "SELECT ") && query != "commit" && query != "rollback" {
			t.Fatal("inspection issued non-read query")
		}
	}
	out, _ := json.Marshal(report)
	for _, secret := range []string{string(p.ApplicationID), string(p.OwnerAccountID), string(p.CredentialIDs[0]), "idempotency"} {
		if strings.Contains(string(out), secret) {
			t.Fatal("inspection leaked scoped data")
		}
	}
	// Explicit positive control: PostgreSQL itself forbids writes in this mode.
	tx, err := reader.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `UPDATE kaana_admission.enrollments SET enabled=enabled`)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "25006" {
		t.Fatal("read-only write control did not fail")
	}
	_ = tx.Rollback(ctx)
}
func snapshotInspectionRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context) string {
	t.Helper()
	var state string
	err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY version) FROM public.kaana_admission_migrations t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM kaana_admission.enrollments t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM kaana_admission.policy_versions t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM kaana_admission.legacy_fences t),
 (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM kaana_admission.fence_imports t)
 )::text`).Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func TestPostgresInspectionReportsAbsenceAndIncompleteLedger(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	report, err := s.InspectPolicy(ctx, p)
	if err != nil || report.MigrationState != "verified" || report.EnrollmentPresent || report.Registered || report.Enabled {
		t.Fatal(report, err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM public.kaana_admission_migrations WHERE version='0002'`); err != nil {
		t.Fatal(err)
	}
	report, err = s.InspectPolicy(ctx, p)
	if err != nil || report.MigrationState != "incomplete" || report.Registered {
		t.Fatal(report, err)
	}
	if _, err = pool.Exec(ctx, `DROP TABLE public.kaana_admission_migrations`); err != nil {
		t.Fatal(err)
	}
	report, err = s.InspectPolicy(ctx, p)
	if err != nil || report.MigrationState != "absent" || report.EnrollmentPresent || report.Registered {
		t.Fatal(report, err)
	}
}
func TestPostgresInspectionRejectsLedgerAndPolicyDrift(t *testing.T) {
	for _, kind := range []string{"checksum", "unknown migration", "policy digest", "policy document", "unknown document field", "different reviewed policy", "active document"} {
		t.Run(kind, func(t *testing.T) {
			s, pool, ctx := testDatabase(t)
			p := policy()
			if err := s.RegisterPolicy(ctx, p); err != nil {
				t.Fatal(err)
			}
			query := ""
			switch kind {
			case "checksum":
				query = `UPDATE public.kaana_admission_migrations SET checksum='bad' WHERE version='0001'`
			case "unknown migration":
				query = `INSERT INTO public.kaana_admission_migrations VALUES('9999','bad')`
			case "policy digest":
				query = `UPDATE kaana_admission.policy_versions SET digest=repeat('0',64)`
			case "policy document":
				query = `UPDATE kaana_admission.policy_versions SET document=jsonb_set(document,'{maxConcurrent}','1')`
			case "unknown document field":
				query = `UPDATE kaana_admission.policy_versions SET document=document||'{"unreviewed":true}'::jsonb`
			case "different reviewed policy":
				p.MaxConcurrent++
			case "active document":
				enableTestPolicy(t, s, pool, ctx, p)
				p.Version++
				if err := s.RegisterPolicy(ctx, p); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE kaana_admission.policy_versions SET document=jsonb_set(document,'{maxConcurrent}','1') WHERE version=1`
			}
			if query != "" {
				if strings.Contains(query, "UPDATE kaana_admission.policy_versions") {
					// Normal writes are immutable. Simulate operator corruption only inside
					// this disposable database to test the independent inspection boundary.
					if _, err := pool.Exec(ctx, query); err == nil {
						t.Fatal("immutable policy control did not reject write")
					}
					if _, err := pool.Exec(ctx, `ALTER TABLE kaana_admission.policy_versions DISABLE TRIGGER immutable_native_policy`); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := pool.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotInspectionRows(t, pool, ctx)
			if _, err := s.InspectPolicy(ctx, p); err == nil {
				t.Fatal("corrupt or conflicting state accepted")
			}
			if before != snapshotInspectionRows(t, pool, ctx) {
				t.Fatal("inspection repaired or changed state")
			}
		})
	}
}

func TestPostgresInspectionOperatorConnectionIsReadOnly(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	if err := s.RegisterPolicy(ctx, policy()); err != nil {
		t.Fatal(err)
	}
	// Inspect the production configuration without connecting to a remote DB,
	// then apply that exact startup mode and role hook to the disposable fixture.
	configured, closeConfigured, err := OpenInspection(ctx, "postgres://inspection@localhost/inspection?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	production := configured.pool.Config()
	closeConfigured()
	if production.MaxConns != 1 || production.ConnConfig.RuntimeParams["default_transaction_read_only"] != "on" {
		t.Fatal("operator connection lost read-only constraint")
	}
	if _, err = pool.Exec(ctx, `GRANT USAGE ON SCHEMA public,kaana_admission TO kaana_migrator; GRANT SELECT ON ALL TABLES IN SCHEMA kaana_admission TO kaana_migrator; GRANT SELECT ON public.kaana_admission_migrations TO kaana_migrator`); err != nil {
		t.Fatal(err)
	}
	config := pool.Config().Copy()
	config.AfterConnect = production.AfterConnect
	config.ConnConfig.RuntimeParams = production.ConnConfig.RuntimeParams
	operator, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	var role, readOnly string
	if err = operator.QueryRow(ctx, `SELECT current_user,current_setting('transaction_read_only')`).Scan(&role, &readOnly); err != nil || role != "kaana_migrator" || readOnly != "on" {
		t.Fatal("operator startup role/read-only proof failed")
	}
	result, err := NewStore(operator).InspectPolicy(ctx, policy())
	if err != nil || !result.Registered || result.Enabled {
		t.Fatal(result, err)
	}
}
