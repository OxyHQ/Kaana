package credentialstore

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acceptingSender answers every call 200, as OpenRouter did before the
// rate-limit frame that sent the 2026-10-01 requests into a same-route retry.
type acceptingSender struct{}

func (acceptingSender) Send(context.Context, *provider.Call, provider.Key) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
}
func (acceptingSender) Refuse(response *http.Response, _ provider.Key) error {
	_ = response.Body.Close()
	return nil
}
func (acceptingSender) TransportFailure(_ context.Context, err error) error { return err }

// TestASameRouteRetryIsItsOwnDurableCredentialAttempt drives two walks of one
// request on one deployment — a same-route retry — through the real
// kaana_record_provider_credential_attempt as kaana_runtime. Sharing the
// request's sequence gives each exchange its own row. The control proves the
// function still refuses the identity reuse that failed production: the same
// (request, deployment, index) observed at another time is a conflict.
func TestASameRouteRetryIsItsOwnDurableCredentialAttempt(t *testing.T) {
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
	t.Cleanup(runtimePool.Close)
	runtime := &Postgres{pool: runtimePool}

	const slug contract.ProviderSlug = "same-route-retry"
	const keyID = "123e4567-e89b-42d3-a456-426614176001"
	deployment := contract.DeploymentID("dep_" + string(slug))
	requestID := contract.RequestID("req_" + string(slug))
	if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,enabled) VALUES($1,$2,'x','arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000001',920000,true) ON CONFLICT(provider_slug,key_id) DO UPDATE SET enabled=true`, slug, keyID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cc()
		_, _ = pool.Exec(c, `DELETE FROM provider_credential_attempt_events WHERE provider_slug=$1`, slug)
		_, _ = pool.Exec(c, `DELETE FROM provider_credential_runtime_state WHERE provider_slug=$1`, slug)
		_, _ = pool.Exec(c, `DELETE FROM provider_credentials WHERE provider_slug=$1`, slug)
	})

	keys, err := provider.NewKeyPool(slug, []provider.KeyDeclaration{{
		KeyID: keyID, Secret: "kaana-integration-fake-credential", Runtime: runtime,
	}}, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sequence := &provider.CredentialAttemptSequence{}
	for range 2 {
		call := &provider.Call{RequestID: requestID, Route: provider.Route{Provider: slug, DeploymentID: deployment}, CredentialAttempts: sequence}
		response, _, err := provider.Walk(ctx, keys, call, acceptingSender{})
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}

	rows, err := pool.Query(ctx, `SELECT credential_attempt_index, outcome FROM provider_credential_attempt_events
		WHERE request_id=$1 AND deployment_id=$2 ORDER BY credential_attempt_index`, requestID, deployment)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []int
	for rows.Next() {
		var (
			index   int
			outcome string
		)
		if err := rows.Scan(&index, &outcome); err != nil {
			t.Fatal(err)
		}
		if outcome != "accepted" {
			t.Errorf("attempt %d recorded as %s", index, outcome)
		}
		recorded = append(recorded, index)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[0] != 0 || recorded[1] != 1 {
		t.Fatalf("recorded attempt indexes %v, want [0 1]: one row per upstream exchange", recorded)
	}

	// Control: what a per-walk index wrote on the retry.
	err = runtime.RecordCredentialAttempt(ctx, provider.CredentialAttempt{
		RequestID: requestID, DeploymentID: deployment, Index: 0, Provider: slug, KeyID: keyID,
		Outcome: "accepted", Evidence: "response_status", OccurredAt: time.Now().Add(time.Second),
	})
	if err == nil || !strings.Contains(err.Error(), "provider credential attempt identity conflict") {
		t.Fatalf("reusing an attempt identity at another time = %v; want the identity conflict", err)
	}
}
