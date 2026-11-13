package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNativeMigrationFlagCannotCombineRestrictedInspection(t *testing.T) {
	for _, args := range [][]string{{"migrate", "--native", "--reviewed-0020"}, {"migrate", "--native", "--reviewed-0021"}, {"migrate", "--native", "--inspect-only"}, {"migrate", "--native", "unexpected"}} {
		err := run(args, strings.NewReader(""), io.Discard, func(string) string { return "invalid private configuration" })
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatal("native flag bypassed restricted migration", err)
		}
	}
}
func TestPostgresNativeMigrationIsExplicitAndNeverEnrolls(t *testing.T) {
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("test database unavailable")
	}
	defer func() { _ = admin.Close(ctx) }()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "native_cli_" + hex.EncodeToString(random[:])
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER kaana_migrator"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	own := config.Copy()
	own.Database = database
	own.RuntimeParams["role"] = "kaana_migrator"
	poolConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid fixture configuration")
	}
	poolConfig.ConnConfig = own
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal("fixture pool unavailable")
	}
	defer pool.Close()
	credentialMigration := func(ctx context.Context) error { return credentialstore.MigrateDatabase(ctx, pool) }
	nativeMigration := func(ctx context.Context) error { return nativeadmission.NewStore(pool).Migrate(ctx) }

	inspect := config.Copy()
	inspect.Database = database
	conn, err := pgx.ConnectConfig(ctx, inspect)
	if err != nil {
		t.Fatal("fixture database unavailable")
	}
	defer func() { _ = conn.Close(ctx) }()
	if err = applySchemaMigrations(ctx, false, credentialMigration, nativeMigration); err != nil {
		t.Fatal(err)
	}
	var absent bool
	if err = conn.QueryRow(ctx, `SELECT to_regnamespace('kaana_admission') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("ordinary migration enabled native schema", err)
	}
	for range 2 {
		if err = applySchemaMigrations(ctx, true, credentialMigration, nativeMigration); err != nil {
			t.Fatal(err)
		}
	}
	var migrations, enrollments, policies, fences int
	var owner string
	if err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.kaana_admission_migrations),(SELECT count(*) FROM kaana_admission.enrollments),(SELECT count(*) FROM kaana_admission.policy_versions),(SELECT count(*) FROM kaana_admission.legacy_fences),pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='kaana_admission'`).Scan(&migrations, &enrollments, &policies, &fences, &owner); err != nil {
		t.Fatal(err)
	}
	if migrations != 2 || enrollments != 0 || policies != 0 || fences != 0 || owner != "kaana_migrator" {
		t.Fatal("schema migration changed enrollment or ownership", migrations, enrollments, policies, fences, owner)
	}
	runtimeConfig := poolConfig.Copy()
	runtimeConfig.ConnConfig.RuntimeParams["role"] = "kaana_runtime"
	runtimePool, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal("runtime pool unavailable")
	}
	defer runtimePool.Close()
	if err = nativeadmission.NewStore(runtimePool).Migrate(ctx); err == nil {
		t.Fatal("runtime performed privileged migration")
	}
}
func TestNativeMigrationFailureStopsTheSchemaSequence(t *testing.T) {
	sentinel := errors.New("migration refused")
	if err := applySchemaMigrations(context.Background(), true, func(context.Context) error { return sentinel }, func(context.Context) error { t.Fatal("native ran after credential failure"); return nil }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := applySchemaMigrations(context.Background(), false, func(context.Context) error { return nil }, func(context.Context) error { t.Fatal("inert migration opened native role"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := applySchemaMigrations(context.Background(), true, func(context.Context) error { return nil }, func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal("native failure suppressed", err)
	}
}
