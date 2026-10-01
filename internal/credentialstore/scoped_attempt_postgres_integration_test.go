package credentialstore

import (
	"context"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScopedAttemptClaimDurableConcurrentAndPrivate(t *testing.T) {
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repo := &Postgres{pool: pool}
	if err = repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Recreate the exact previous function, snapshot owner/ACL/security configuration,
	// then replace only its body as the migration does. No grant is used.
	previous := migration0011[strings.Index(migration0011, "CREATE FUNCTION kaana_record_provider_credential_attempt("):strings.Index(migration0011, "CREATE FUNCTION kaana_reset_provider_credential_runtime")]
	previous = strings.Replace(previous, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
	if _, err = pool.Exec(ctx, previous); err != nil {
		t.Fatal(err)
	}
	const aclSQL = `SELECT concat(proowner::text,'|',proacl::text,'|',prosecdef::text,'|',proconfig::text) FROM pg_proc WHERE oid='kaana_record_provider_credential_attempt(text,text,integer,text,text,text,text,timestamp with time zone,timestamp with time zone)'::regprocedure`
	var before, after string
	if err = pool.QueryRow(ctx, aclSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	replacement := migration0020[strings.Index(migration0020, "CREATE OR REPLACE FUNCTION"):]
	if _, err = pool.Exec(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, aclSQL).Scan(&after); err != nil || before != after {
		t.Fatal("recorder authority changed", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,key_class,enabled) VALUES('claimfixture','exact','\x01',$1,1,'paid',true)`, kmsTestKeyARN)
	if err != nil {
		t.Fatal(err)
	}
	var binding string
	err = pool.QueryRow(ctx, `SELECT kaana_bind_provider_deployment('kdb_000000000000000000000000000000ab','dep_claimfixture','claimfixture','exact','synthetic-test')`).Scan(&binding)
	if err != nil || binding != "applied" {
		t.Fatal(err, binding)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET SESSION AUTHORIZATION kaana_runtime`)
		return err
	}
	runtimePool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	runtime := &Postgres{pool: runtimePool}
	now := time.Now().UTC()
	claim := ScopedAttemptClaim{PermitID: "req_scoped_claimfixture", DeploymentID: contract.DeploymentID("dep_claimfixture"), Scope: Scope{Provider: "claimfixture", KeyID: "exact"}, ClaimedAt: now, ExpiresAt: now.Add(time.Minute)}
	var wins atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := runtime.ClaimScopedAttempt(ctx, claim)
			if err != nil {
				failures.Add(1)
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || failures.Load() != 0 {
		t.Fatalf("wins=%d failures=%d", wins.Load(), failures.Load())
	}
	restarted := &Postgres{pool: runtimePool}
	if won, err := restarted.ClaimScopedAttempt(ctx, claim); err != nil || won {
		t.Fatal("replay authorized", err)
	}
	conflict := claim
	conflict.DeploymentID = "different"
	if won, _ := runtime.ClaimScopedAttempt(ctx, conflict); won {
		t.Fatal("conflict authorized")
	}
	expired := claim
	expired.PermitID = "req_expired"
	expired.ExpiresAt = now.Add(-time.Second)
	if won, err := runtime.ClaimScopedAttempt(ctx, expired); won || err == nil {
		t.Fatal("expired authorized")
	}
	var events int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM provider_credential_attempt_events WHERE request_id=$1`, claim.PermitID).Scan(&events)
	if err != nil || events != 0 {
		t.Fatal("claim fabricated outcome", err, events)
	}
	var canSelect, canInsert bool
	err = pool.QueryRow(ctx, `SELECT has_table_privilege('kaana_runtime','scoped_provider_attempt_claims','SELECT'),has_table_privilege('kaana_runtime','scoped_provider_attempt_claims','INSERT')`).Scan(&canSelect, &canInsert)
	if err != nil || canSelect || canInsert {
		t.Fatal("runtime table authority widened", err)
	}
}
