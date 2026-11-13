package commercialadmission

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCommercialRuntimeRequiresVerifiedDatabaseConfiguration(t *testing.T) {
	for _, input := range []string{"", "postgres://user:private@localhost/test?sslmode=disable", "postgres://user:private@localhost/test?sslmode=require"} {
		_, err := OpenRuntime(context.Background(), input)
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("unverified connection accepted or secret exposed")
		}
	}
}

// Keep TLS validation on the production boundary. Local CI PostgreSQL has no
// certificate, so exercise the exact role hook and constructor against it with
// a disposable nonsuperuser login instead of weakening production TLS.
func runtimeFixture(t *testing.T, f *fixture) (*Runtime, *pgxpool.Config, string) {
	t.Helper()
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err = tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-test-role-provision'))`); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = tx.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='kaana_commercial_runtime')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err = tx.Exec(f.ctx, `CREATE ROLE kaana_commercial_runtime NOLOGIN INHERIT`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(f.ctx, `GRANT kaana_admission_runtime,kaana_billing_runtime TO kaana_commercial_runtime WITH INHERIT TRUE, SET FALSE, ADMIN FALSE`); err != nil {
		t.Fatal(err)
	}
	login := testID("commercial_login_")
	password := testID("ephemeral_")
	if _, err = tx.Exec(f.ctx, `CREATE ROLE `+pgx.Identifier{login}.Sanitize()+` LOGIN NOINHERIT PASSWORD '`+password+`'`); err != nil {
		t.Fatal("could not provision disposable login")
	}
	if _, err = tx.Exec(f.ctx, `GRANT kaana_commercial_runtime TO `+pgx.Identifier{login}.Sanitize()+` WITH INHERIT FALSE, SET TRUE, ADMIN FALSE`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.ctx, `GRANT CONNECT ON DATABASE `+pgx.Identifier{f.db.Config().ConnConfig.Database}.Sanitize()+` TO `+pgx.Identifier{login}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.db.Exec(context.Background(), `REVOKE CONNECT ON DATABASE `+pgx.Identifier{f.db.Config().ConnConfig.Database}.Sanitize()+` FROM `+pgx.Identifier{login}.Sanitize()+`; DROP ROLE `+pgx.Identifier{login}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	production, err := runtimeConfig("postgres://unused@localhost/unused?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	local := f.db.Config().Copy()
	local.ConnConfig.User, local.ConnConfig.Password = login, password
	local.AfterConnect = production.AfterConnect
	runtime, err := openConfiguredRuntime(f.ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	return runtime, local, login
}

func TestPostgresServingCommercialRuntimeRecoversOnceWithoutAdministrativeGrants(t *testing.T) {
	f := database(t)
	runtime, config, _ := runtimeFixture(t, f)
	// An enabled account with no funds still cannot authorize execution.
	if _, err := runtime.Store.Admit(f.ctx, f.request(t, "unfunded-serving")); !errors.Is(err, billing.ErrFunds) {
		t.Fatalf("unfunded request: %v", err)
	}
	f.count(t, "kaana_admission.admissions", 0)
	f.fund(t)
	a, err := runtime.Store.Admit(f.ctx, f.request(t, "serving-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Store.StageCompletion(f.ctx, a.ID, report(a), nil); err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	restarted, err := openConfiguredRuntime(f.ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	for range 2 {
		if err = restarted.Recover(f.ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	f.count(t, "kaana_billing.receipts", 1)
	f.count(t, "kaana_admission.settlements", 1)
	for _, query := range []string{
		`UPDATE kaana_admission.enrollments SET enabled=false`,
		`INSERT INTO kaana_billing.accounts DEFAULT VALUES`,
		`SELECT kaana_billing.create_account('not-authorized','USD')`,
		`SELECT kaana_billing.fund('missing','key','purchased','1','unverified')`,
		`CREATE TABLE kaana_commercial.runtime_must_not_create(id int)`,
		`SET ROLE kaana_billing_admin`,
	} {
		if _, err = restarted.pool.Exec(f.ctx, query); err == nil {
			t.Fatal("serving role acquired administrative permission")
		}
	}
}

func TestPostgresServingCommercialRuntimeRejectsAdministrativeLoginAndMissingSchema(t *testing.T) {
	f := database(t)
	runtime, config, login := runtimeFixture(t, f)
	runtime.Close()
	if _, err := f.db.Exec(f.ctx, `GRANT kaana_billing_admin TO `+pgx.Identifier{login}.Sanitize()+` WITH INHERIT FALSE, SET TRUE`); err != nil {
		t.Fatal(err)
	}
	if got, err := openConfiguredRuntime(f.ctx, config); err == nil {
		got.Close()
		t.Fatal("serving login with SET administrative permission accepted")
	}
	if _, err := f.db.Exec(f.ctx, `REVOKE kaana_billing_admin FROM `+pgx.Identifier{login}.Sanitize()+`; DROP TABLE kaana_commercial.outcomes`); err != nil {
		t.Fatal(err)
	}
	if got, err := openConfiguredRuntime(f.ctx, config); err == nil {
		got.Close()
		t.Fatal("incomplete commercial schema accepted")
	}
}

func TestPostgresServingCommercialRuntimeRejectsRoleDelegation(t *testing.T) {
	f := database(t)
	runtime, config, login := runtimeFixture(t, f)
	runtime.Close()
	// Keep global role changes isolated from other disposable PG fixtures using
	// the same provisioning lock. Each option is restored before releasing it.
	operator, err := f.db.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Release()
	if _, err = operator.Exec(f.ctx, `SELECT pg_advisory_lock(hashtext('billing-test-role-provision'))`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := operator.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('billing-test-role-provision'))`); err != nil {
			t.Error(err)
		}
	}()
	for _, grant := range []struct{ role, member string }{
		{"kaana_commercial_runtime", login},
		{"kaana_admission_runtime", "kaana_commercial_runtime"},
		{"kaana_billing_runtime", "kaana_commercial_runtime"},
	} {
		t.Run(grant.role, func(t *testing.T) {
			role, member := pgx.Identifier{grant.role}.Sanitize(), pgx.Identifier{grant.member}.Sanitize()
			if _, err := operator.Exec(f.ctx, `GRANT `+role+` TO `+member+` WITH ADMIN OPTION`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := operator.Exec(context.Background(), `REVOKE ADMIN OPTION FOR `+role+` FROM `+member); err != nil {
					t.Error(err)
				}
			}()
			var elevated bool
			if err := operator.QueryRow(f.ctx, `SELECT bool_or(admin_option) FROM pg_auth_members WHERE roleid=$1::regrole AND member=$2::regrole`, grant.role, grant.member).Scan(&elevated); err != nil || !elevated {
				t.Fatal("negative control did not acquire delegation authority", err)
			}
			if got, err := openConfiguredRuntime(f.ctx, config); err == nil {
				got.Close()
				t.Fatal("runtime accepted ADMIN OPTION")
			}
			if _, err := operator.Exec(f.ctx, `REVOKE ADMIN OPTION FOR `+role+` FROM `+member); err != nil {
				t.Fatal(err)
			}
			control, err := openConfiguredRuntime(f.ctx, config)
			if err != nil {
				t.Fatal("SET-only runtime refused after removing delegation authority", err)
			}
			control.Close()
		})
	}
}

func TestPostgresServingInternalMeteringDoesNotReserveCommercialFunds(t *testing.T) {
	f := database(t)
	f.policy.Version++
	f.policy.EconomicTreatment = nativeadmission.InternalMetered
	f.activate(t, f.policy)
	runtime, _, _ := runtimeFixture(t, f)
	a, err := runtime.Store.Admit(f.ctx, f.request(t, "internal-serving"))
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Store.StageCompletion(f.ctx, a.ID, report(a), nil); err != nil {
		t.Fatal(err)
	}
	if err = runtime.Recover(f.ctx, 100); err != nil {
		t.Fatal(err)
	}
	f.count(t, "kaana_billing.holds", 0)
	f.count(t, "kaana_billing.receipts", 0)
	f.count(t, "kaana_admission.settlements", 1)
}
