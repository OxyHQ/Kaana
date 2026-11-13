package commercialadmission

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPostgresCommercialMigrationUsesExplicitOperatorAndVerifiedNativePrerequisite(t *testing.T) {
	f := database(t)
	runtime, config, login := runtimeFixture(t, f)
	runtime.Close()
	if _, err := f.db.Exec(f.ctx, `GRANT kaana_billing_migrator TO `+pgx.Identifier{login}.Sanitize()+` WITH INHERIT FALSE, SET TRUE;
 DROP SCHEMA kaana_commercial CASCADE; DROP SCHEMA kaana_billing CASCADE;
 DROP TABLE public.kaana_commercial_migrations,public.kaana_billing_migrations`); err != nil {
		t.Fatal(err)
	}
	production, err := migrationConfig("postgres://unused@localhost/unused?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = production.AfterConnect
	if err = migrateConfigured(f.ctx, config); err == nil {
		t.Fatal("operator without native ledger read authority accepted")
	}
	var absent bool
	if err = f.db.QueryRow(f.ctx, `SELECT to_regclass('kaana_billing.accounts') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("financial schema created before native prerequisite verification", err)
	}
	if _, err = f.db.Exec(f.ctx, `GRANT SELECT ON public.kaana_admission_migrations TO kaana_billing_migrator`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = migrateConfigured(f.ctx, config); err != nil {
			t.Fatal(err)
		}
	}
	f.count(t, "kaana_billing.accounts", 0)
	f.count(t, "kaana_billing.currencies", 0)
	f.count(t, "kaana_commercial.admissions", 0)
	f.count(t, "kaana_admission.enrollments", 1)
	for _, table := range []string{"kaana_admission_migrations", "kaana_billing_migrations", "kaana_commercial_migrations"} {
		var checksum string
		if err = f.db.QueryRow(f.ctx, `SELECT checksum FROM public.`+table+` WHERE version='0001'`).Scan(&checksum); err != nil {
			t.Fatal(err)
		}
		if _, err = f.db.Exec(f.ctx, `UPDATE public.`+table+` SET checksum='changed' WHERE version='0001'`); err != nil {
			t.Fatal(err)
		}
		if err = migrateConfigured(f.ctx, config); err == nil {
			t.Fatal("changed migration checksum accepted", table)
		}
		if _, err = f.db.Exec(f.ctx, `UPDATE public.`+table+` SET checksum=$1 WHERE version='0001'`, checksum); err != nil {
			t.Fatal(err)
		}
		if _, err = f.db.Exec(f.ctx, `INSERT INTO public.`+table+` VALUES ('future','unreviewed')`); err != nil {
			t.Fatal(err)
		}
		if err = migrateConfigured(f.ctx, config); err == nil {
			t.Fatal("unknown future migration accepted", table)
		}
		if _, err = f.db.Exec(f.ctx, `DELETE FROM public.`+table+` WHERE version='future'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.db.Exec(f.ctx, `REVOKE REFERENCES ON kaana_admission.admissions FROM kaana_billing_migrator`); err != nil {
		t.Fatal(err)
	}
	if err = migrateConfigured(f.ctx, config); err == nil {
		t.Fatal("migration accepted without native reference authority")
	}
}
