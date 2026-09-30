package credentialstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPublicationEvidencePostgres reads the publisher's evidence AS
// kaana_runtime — the role the publisher runs under — so a grant missing from
// migration 0019 fails here rather than in the first production cycle.
func TestPublicationEvidencePostgres(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if err := (&Postgres{pool: admin}).Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	runtimeConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE kaana_runtime`)
		return err
	}
	runtimePool, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtimePool.Close)
	runtime := &Postgres{pool: runtimePool}

	// A slug of its own per run: other suites truncate these tables, and the
	// evidence ledger is append-only.
	suffix := time.Now().UnixNano()
	slug := contract.ProviderSlug(fmt.Sprintf("pubtest-%d", suffix))
	other := contract.ProviderSlug(fmt.Sprintf("pubother-%d", suffix))
	now := time.Now().UTC().Truncate(time.Microsecond)
	arn := "arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111"
	exec := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, statement, arguments...); err != nil {
			t.Fatalf("seeding %q: %v", statement, err)
		}
	}
	credential := `INSERT INTO provider_credentials (provider_slug, key_id, encrypted_secret, kms_key_arn, position, enabled, updated_at)
		VALUES ($1, $2, '\x01', $3, $4, $5, $6)`
	exec(credential, slug, "key-a", arn, 1, true, now.Add(-2*time.Hour))
	// key-b was rotated ten minutes ago: its older failures describe a secret
	// that is no longer in the row.
	exec(credential, slug, "key-b", arn, 2, true, now.Add(-10*time.Minute))
	exec(credential, slug, "key-off", arn, 3, false, now.Add(-2*time.Hour))
	exec(credential, other, "key-o", arn, 1, true, now.Add(-2*time.Hour))

	exec(`INSERT INTO provider_credential_runtime_state (provider_slug, key_id, state, evidence_source, retired_until, observed_at)
		VALUES ($1, 'key-b', 'exhausted', 'provider_error', $2, $3)`, slug, now.Add(15*time.Minute), now.Add(-time.Minute))
	exec(`INSERT INTO provider_credential_capacity_evidence (evidence_id, provider_slug, key_id, kind, source, observed_at,
		fresh_until, balance_currency, balance_picos, operation_actor)
		VALUES ($1, $2, 'key-a', 'balance', 'provider_console', $3, $4, 'USD', 0, 'integration-test')`,
		fmt.Sprintf("kce_%032x", suffix), slug, now.Add(-time.Minute), now.Add(time.Hour))

	operation := fmt.Sprintf("kdb_%032x", suffix)
	exec(`INSERT INTO provider_deployment_binding_operations (operation_id, deployment_id, provider_slug, key_id, operation_actor, database_actor)
		VALUES ($1, $2, $3, 'key-a', 'integration-test', 'integration-test')`, operation, "dep_bound_"+string(slug), slug)
	exec(`INSERT INTO provider_deployment_credential_bindings (deployment_id, provider_slug, key_id, last_operation_id, operation_actor)
		VALUES ($1, $2, 'key-a', $3, 'integration-test')`, "dep_bound_"+string(slug), slug, operation)

	depX := contract.DeploymentID("dep_x_" + string(slug))
	depZ := contract.DeploymentID("dep_z_" + string(slug))
	attempt := `INSERT INTO provider_cost_events (request_id, attempt_index, provider_slug, key_id, deployment_id,
		model_reference, source, complete, served, occurred_at, created_at,
		usage_units, attempt_started_at, latency_ms, attempt_outcome, failure_code)
		VALUES ($1, 0, $2, $3, $4, 'openai/model@observed-2026-09-01', 'unknown', false, $5, $6, $6,
		'[]', $6, 10, $7, $8)`
	sequence := 0
	record := func(provider contract.ProviderSlug, keyID string, deployment contract.DeploymentID, ago time.Duration, outcome string, code any) {
		t.Helper()
		sequence++
		exec(attempt, fmt.Sprintf("req_%d_%d", suffix, sequence), provider, keyID, deployment, outcome == "succeeded", now.Add(-ago), outcome, code)
	}
	// dep_x on key-a: a failure before its last success, the success, then the
	// streak — two server errors and a throttle — and a cancellation.
	record(slug, "key-a", depX, 55*time.Minute, "failed", "provider_error")
	record(slug, "key-a", depX, 50*time.Minute, "succeeded", nil)
	record(slug, "key-a", depX, 40*time.Minute, "failed", "provider_error")
	record(slug, "key-a", depX, 30*time.Minute, "failed", "provider_error")
	record(slug, "key-a", depX, 20*time.Minute, "failed", "rate_limited")
	record(slug, "key-a", depX, 15*time.Minute, "cancelled", nil)
	// dep_z on the rotated key-b: only the refusal after the rotation counts.
	record(slug, "key-b", depZ, 15*time.Minute, "failed", "provider_billing_refused")
	record(slug, "key-b", depZ, 5*time.Minute, "failed", "provider_billing_refused")
	// Outside the window, on a disabled key, and for a provider nobody asked.
	record(slug, "key-a", depX, 3*time.Hour, "failed", "provider_error")
	record(slug, "key-off", depX, 5*time.Minute, "failed", "provider_error")
	record(other, "key-o", "dep_other", 5*time.Minute, "failed", "provider_error")

	evidence, err := runtime.ReadPublicationEvidence(ctx, []contract.ProviderSlug{slug}, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("reading publication evidence as kaana_runtime: %v", err)
	}

	if len(evidence.Keys) != 2 || evidence.Keys[0].KeyID != "key-a" || evidence.Keys[1].KeyID != "key-b" {
		t.Fatalf("keys = %+v; want the two enabled keys in position order", evidence.Keys)
	}
	if evidence.Keys[0].Runtime.Reason != "" || evidence.Keys[1].Runtime.Reason != provider.KeyExhausted ||
		!evidence.Keys[1].Runtime.RetiredUntil.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("runtime state = %+v / %+v", evidence.Keys[0].Runtime, evidence.Keys[1].Runtime)
	}
	capacity := evidence.Keys[0].Capacity
	if len(capacity) != 1 || capacity[0].Kind != "balance" || !capacity[0].Empty() ||
		!capacity[0].FreshUntil.Equal(now.Add(time.Hour)) || !capacity[0].ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("capacity = %+v", capacity)
	}
	if len(evidence.Bindings) != 1 || evidence.Bindings[0].KeyID != "key-a" || evidence.Bindings[0].DeploymentID != "dep_bound_"+contract.DeploymentID(slug) {
		t.Fatalf("bindings = %+v", evidence.Bindings)
	}

	type group struct {
		deployment contract.DeploymentID
		key        string
		code       contract.ErrorCode
		count      int
		first      time.Duration
		last       time.Duration
	}
	want := []group{
		{depX, "key-a", contract.CodeProviderError, 2, 40 * time.Minute, 30 * time.Minute},
		{depX, "key-a", contract.CodeRateLimited, 1, 20 * time.Minute, 20 * time.Minute},
		{depZ, "key-b", contract.CodeProviderBillingRefused, 1, 5 * time.Minute, 5 * time.Minute},
	}
	if len(evidence.Failures) != len(want) {
		t.Fatalf("streaks = %+v; want %d groups", evidence.Failures, len(want))
	}
	for index, expected := range want {
		got := evidence.Failures[index]
		if got.DeploymentID != expected.deployment || got.KeyID != expected.key || got.FailureCode != expected.code ||
			got.Failures != expected.count || !got.FirstAt.Equal(now.Add(-expected.first)) || !got.LastAt.Equal(now.Add(-expected.last)) {
			t.Errorf("streak %d = %+v, want %+v", index, got, expected)
		}
	}

	// A success after the streak ends it.
	record(slug, "key-a", depX, 10*time.Minute, "succeeded", nil)
	evidence, err = runtime.ReadPublicationEvidence(ctx, []contract.ProviderSlug{slug}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, streak := range evidence.Failures {
		if streak.DeploymentID == depX {
			t.Errorf("a success did not end dep_x's streak: %+v", streak)
		}
	}

	// The window is bounded so no caller can scan the whole table.
	if _, err := runtime.ReadPublicationEvidence(ctx, []contract.ProviderSlug{slug}, now.Add(-9*24*time.Hour)); err == nil {
		t.Error("a nine-day window was read")
	}

	// And the runtime reads the attempts only through that function.
	var granted bool
	if err := admin.QueryRow(ctx, `SELECT has_table_privilege('kaana_runtime', 'provider_cost_events', 'SELECT')`).Scan(&granted); err != nil || granted {
		t.Errorf("kaana_runtime SELECT on provider_cost_events = %v/%v", granted, err)
	}
	if err := admin.QueryRow(ctx, `SELECT has_function_privilege('kaana_credential_admin', 'kaana_read_deployment_failure_streaks(timestamptz)', 'EXECUTE')`).Scan(&granted); err != nil || granted {
		t.Errorf("kaana_credential_admin EXECUTE on the streak read = %v/%v; only the runtime needs it", granted, err)
	}
}
