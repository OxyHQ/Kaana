package credentialstore

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeploymentBindingPostgresLifecycle(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const providerSlug = "binding-integration"
	keys := []string{"123e4567-e89b-42d3-a456-426614174001", "123e4567-e89b-42d3-a456-426614174002"}
	for index, key := range keys {
		_, err := pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,enabled) VALUES($1,$2,'x','arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000001',$3,true) ON CONFLICT(provider_slug,key_id) DO UPDATE SET enabled=true`, providerSlug, key, index+900000)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = pool.Exec(c, `DELETE FROM provider_deployment_credential_bindings WHERE provider_slug=$1`, providerSlug)
		_, _ = pool.Exec(c, `DELETE FROM provider_deployment_binding_operations WHERE provider_slug=$1`, providerSlug)
		_, _ = pool.Exec(c, `DELETE FROM provider_credentials WHERE provider_slug=$1`, providerSlug)
	})
	binding := provider.CredentialBinding{DeploymentID: "dep_binding_integration", Provider: contract.ProviderSlug(providerSlug), KeyID: keys[0]}
	outcome, err := repository.BindDeployment(ctx, "kdb_0123456789abcdef0123456789abcdef", binding, "operator:integration")
	if err != nil || outcome != "applied" {
		t.Fatalf("applied=%q err=%v", outcome, err)
	}
	outcome, err = repository.BindDeployment(ctx, "kdb_0123456789abcdef0123456789abcdef", binding, "operator:integration")
	if err != nil || outcome != "replayed" {
		t.Fatalf("replayed=%q err=%v", outcome, err)
	}
	conflict := binding
	conflict.KeyID = keys[1]
	if _, err := repository.BindDeployment(ctx, "kdb_0123456789abcdef0123456789abcdef", conflict, "operator:integration"); err == nil {
		t.Fatal("conflicting replay succeeded")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	failures := make(chan error, 2)
	for _, candidate := range keys {
		go func(key string) {
			defer wg.Done()
			b := binding
			b.DeploymentID = "dep_binding_concurrent"
			b.KeyID = key
			_, e := repository.BindDeployment(ctx, "kdb_abcdef0123456789abcdef0123456789", b, "operator:integration")
			failures <- e
		}(candidate)
	}
	wg.Wait()
	close(failures)
	successes := 0
	conflicts := 0
	for failure := range failures {
		if failure == nil {
			successes++
		} else if errors.Is(failure, ErrDeploymentBindingConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent error: %v", failure)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent outcomes successes=%d conflicts=%d", successes, conflicts)
	}
	metadata, err := repository.ListDeploymentBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range metadata {
		if item.DeploymentID == binding.DeploymentID {
			found = true
			if item.KeyID != keys[0] || item.OperationID != "kdb_0123456789abcdef0123456789abcdef" || item.OperationActor != "operator:integration" || item.DatabaseActor == "" {
				t.Fatalf("metadata=%+v", item)
			}
		}
	}
	if !found {
		t.Fatal("binding readback missing")
	}
	// Exercise the query used at boot with the serving identity, not the
	// migrator/superuser that created this fixture. A bare table-read probe
	// misses permissions required by a join in LoadDeploymentBindings.
	runtimeConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET SESSION AUTHORIZATION kaana_runtime`)
		return err
	}
	runtimePool, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	runtimeRepository := &Postgres{pool: runtimePool}
	active, err := runtimeRepository.LoadDeploymentBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, item := range active {
		if item == binding {
			found = true
		}
	}
	if !found {
		t.Fatal("runtime did not load the enabled exact binding")
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_credentials SET enabled=false WHERE provider_slug=$1 AND key_id=$2`, providerSlug, keys[0]); err != nil {
		t.Fatal(err)
	}
	live, err := runtimeRepository.LoadDeploymentBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range live {
		if b.DeploymentID == binding.DeploymentID {
			t.Fatal("disabled credential remained executable")
		}
	}
	bad := binding
	bad.Provider = "wrong-provider"
	if _, err := repository.BindDeployment(ctx, "kdb_11111111111111111111111111111111", bad, "operator:integration"); err == nil {
		t.Fatal("provider mismatch was accepted")
	}
	assertDeploymentBindingRoleBoundaries(t, ctx, pool, providerSlug, keys[1])
}

func assertDeploymentBindingRoleBoundaries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, providerSlug, keyID string) {
	t.Helper()
	const publicRole = "kaana_binding_public_test"
	if _, err := pool.Exec(ctx, `DO $do$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='kaana_binding_public_test') THEN CREATE ROLE kaana_binding_public_test NOLOGIN; END IF; END $do$`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupContext, `DROP ROLE IF EXISTS kaana_binding_public_test`)
	})

	acquireAs := func(role string) *pgxpool.Conn {
		connection, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION `+pgx.Identifier{role}.Sanitize()); err != nil {
			connection.Release()
			t.Fatal(err)
		}
		return connection
	}
	releaseAs := func(connection *pgxpool.Conn) {
		if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION`); err != nil {
			_ = connection.Conn().Close(ctx)
		}
		connection.Release()
	}
	denied := func(name string, connection *pgxpool.Conn, statement string, args ...any) {
		t.Helper()
		if _, err := connection.Exec(ctx, statement, args...); err == nil {
			t.Errorf("%s unexpectedly succeeded", name)
		}
	}

	admin := acquireAs("kaana_credential_admin")
	var state string
	if err := admin.QueryRow(ctx, `SELECT kaana_bind_provider_deployment($1,$2,$3,$4,$5)`, "kdb_22222222222222222222222222222222", "dep_admin_role", providerSlug, keyID, "operator:role-test").Scan(&state); err != nil || state != "applied" {
		t.Fatalf("admin execute state=%q err=%v", state, err)
	}
	var actor string
	if err := admin.QueryRow(ctx, `SELECT database_actor FROM provider_deployment_binding_metadata WHERE deployment_id='dep_admin_role'`).Scan(&actor); err != nil || actor != "kaana_credential_admin" {
		t.Fatalf("admin readback actor=%q err=%v", actor, err)
	}
	for _, statement := range []string{
		`INSERT INTO provider_deployment_credential_bindings(deployment_id,provider_slug,key_id,last_operation_id,operation_actor) VALUES('forbidden',$1,$2,'kdb_22222222222222222222222222222222','forbidden')`,
		`UPDATE provider_deployment_credential_bindings SET key_id=$2 WHERE provider_slug=$1`,
		`DELETE FROM provider_deployment_credential_bindings WHERE provider_slug=$1`,
		`UPDATE provider_deployment_binding_operations SET operation_actor='forbidden' WHERE provider_slug=$1`,
	} {
		denied("admin direct binding/history DML", admin, statement, providerSlug, keyID)
	}
	denied("admin provider ciphertext read", admin, `SELECT encrypted_secret FROM provider_credentials LIMIT 1`)
	releaseAs(admin)

	runtime := acquireAs("kaana_runtime")
	var runtimeKey string
	if err := runtime.QueryRow(ctx, `SELECT key_id FROM provider_deployment_credential_bindings WHERE deployment_id='dep_admin_role'`).Scan(&runtimeKey); err != nil || runtimeKey != keyID {
		t.Fatalf("runtime binding read key=%q err=%v", runtimeKey, err)
	}
	denied("runtime operation history", runtime, `SELECT * FROM provider_deployment_binding_operations LIMIT 1`)
	denied("runtime provider ciphertext read", runtime, `SELECT encrypted_secret FROM provider_credentials LIMIT 1`)
	denied("runtime admin execute", runtime, `SELECT kaana_bind_provider_deployment('kdb_33333333333333333333333333333333','dep_forbidden',$1,$2,'operator:forbidden')`, providerSlug, keyID)
	denied("runtime binding DML", runtime, `DELETE FROM provider_deployment_credential_bindings WHERE deployment_id='dep_admin_role'`)
	releaseAs(runtime)

	public := acquireAs(publicRole)
	denied("PUBLIC binding read", public, `SELECT * FROM provider_deployment_credential_bindings LIMIT 1`)
	denied("PUBLIC operation read", public, `SELECT * FROM provider_deployment_binding_operations LIMIT 1`)
	denied("PUBLIC admin execute", public, `SELECT kaana_bind_provider_deployment('kdb_44444444444444444444444444444444','dep_forbidden',$1,$2,'operator:forbidden')`, providerSlug, keyID)
	releaseAs(public)
}
