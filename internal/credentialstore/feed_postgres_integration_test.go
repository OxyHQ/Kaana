package credentialstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProviderTelemetryFeedPostgres(t *testing.T) {
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
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE provider_cost_events, provider_credential_admin_operations,
		provider_credential_audit, provider_credentials RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("resetting: %v", err)
	}
	for _, statement := range []string{
		`INSERT INTO provider_credentials (provider_slug, key_id, encrypted_secret, kms_key_arn, position, key_class)
		 VALUES ('feedtest', 'key-free', '\x01', 'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', 1, 'free')`,
		`INSERT INTO provider_credentials (provider_slug, key_id, encrypted_secret, kms_key_arn, position, key_class, enabled)
		 VALUES ('feedtest', 'key-disabled', '\x01', 'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', 2, 'paid', FALSE)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seeding credentials: %v", err)
		}
	}
	// Three settled attempts (one from before telemetry existed) and one that
	// has not settled yet. created_at is set directly: the feed's settle
	// window is what is under test, not the clock.
	insert := `INSERT INTO provider_cost_events (request_id, attempt_index, provider_slug, key_id, deployment_id,
		model_reference, currency, amount_picos, source, complete, served, occurred_at, created_at,
		usage_units, attempt_started_at, latency_ms, time_to_first_output_ms, attempt_outcome, failure_code)
		VALUES ($1, $2, 'feedtest', 'key-free', 'dep_a', 'openai/model@2026-09-01', $3, $4, $5, $6, $7,
		NOW() - INTERVAL '1 hour', $8, $9, $10, $11, $12, $13, $14)`
	past := time.Now().Add(-time.Hour).UTC()
	started := past.Add(-time.Second)
	usd, amount := "USD", int64(125000)
	units := `[{"unit":"input_tokens","quantity":12}]`
	latency, ttft, succeeded, failed, throttled := 900, 250, "succeeded", "failed", "rate_limited"
	for _, row := range [][]any{
		{"req_legacy", 0, nil, nil, "unknown", false, true, past, nil, nil, nil, nil, nil, nil},
		{"req_b", 0, nil, nil, "unknown", false, false, past.Add(time.Second), units, started, latency, nil, failed, throttled},
		{"req_b", 1, usd, amount, "provider_reported", true, true, past.Add(time.Second), units, started, latency, ttft, succeeded, nil},
		{"req_empty", 0, nil, nil, "unknown", false, false, past.Add(2 * time.Second), "[]", started, latency, nil, failed, throttled},
		{"req_unsettled", 0, nil, nil, "unknown", false, true, time.Now().UTC(), units, started, latency, nil, succeeded, nil},
	} {
		if _, err := pool.Exec(ctx, insert, row...); err != nil {
			t.Fatalf("seeding attempt %v: %v", row[:2], err)
		}
	}

	page, next, err := repository.ReadAttemptFeed(ctx, nil, 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page) != 2 || page[0].RequestID != "req_legacy" || page[1].RequestID != "req_b" || page[1].AttemptIndex != 0 {
		t.Fatalf("first page = %+v", page)
	}
	if page[0].Telemetry != nil || page[0].Units != nil {
		t.Fatalf("an unmeasured legacy attempt was given telemetry: %+v", page[0])
	}
	wire, err := json.Marshal(page[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["units"]) != "null" {
		t.Fatalf("SQL NULL measurement serialized as %s, want null", decoded["units"])
	}
	if page[1].KeyClass != "free" || page[1].Telemetry == nil || page[1].Telemetry.Outcome != "failed" ||
		page[1].Telemetry.FailureCode == nil || *page[1].Telemetry.FailureCode != "rate_limited" || page[1].Cost != nil {
		t.Fatalf("throttled attempt = %+v / %+v", page[1], page[1].Telemetry)
	}
	resumed, err := ParseAttemptFeedCursor(next.Encode())
	if err != nil {
		t.Fatalf("the feed's own cursor does not parse: %v", err)
	}
	rest, last, err := repository.ReadAttemptFeed(ctx, &resumed, 10)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(rest) != 2 || rest[0].RequestID != "req_b" || rest[0].AttemptIndex != 1 ||
		rest[0].Cost == nil || rest[0].Cost.AmountPicos != "125000" || *rest[0].Telemetry.TimeToFirstOutputMs != 250 {
		t.Fatalf("second page = %+v (the unsettled attempt must not appear yet)", rest)
	}
	if rest[1].RequestID != "req_empty" || rest[1].Units == nil || len(rest[1].Units) != 0 {
		t.Fatalf("measured empty units did not survive SQL: %+v", rest[1])
	}
	wire, err = json.Marshal(rest[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["units"]) != "[]" {
		t.Fatalf("measured empty units serialized as %s, want []", decoded["units"])
	}
	empty, unchanged, err := repository.ReadAttemptFeed(ctx, last, 10)
	if err != nil || len(empty) != 0 || unchanged.Encode() != last.Encode() {
		t.Fatalf("caught-up read = %+v, %v, %v", empty, unchanged, err)
	}

	// Funding accounts and description operations are append-only, so each
	// run describes the key under fresh identities.
	suffix := fmt.Sprintf("%032x", time.Now().UnixNano())
	account := "kfa_" + suffix
	if _, err := repository.PutCredentialMetadata(ctx, CredentialMetadata{
		SchemaVersion: 1, OperationID: "kcm_" + suffix, Provider: contract.ProviderSlug("feedtest"),
		KeyID: "key-free", Title: "Feed key", FundingAccount: FundingAccount{ID: account, Label: "owner@example.com"},
		CapacityCategory: CapacityPromotional, Environment: "production",
		CommercialUse: CommercialUseClaim{Eligibility: CommercialUseNotPermitted, Evidence: "promotion terms, clause 4"},
	}, "integration-test"); err != nil {
		t.Fatalf("describing key: %v", err)
	}
	economics, err := repository.ReadCredentialEconomics(ctx)
	if err != nil {
		t.Fatalf("economics: %v", err)
	}
	if len(economics) != 1 || economics[0].KeyID != "key-free" || economics[0].Description == nil ||
		economics[0].Description.CapacityCategory != "promotional" || economics[0].Description.CommercialUse != "not_permitted" ||
		economics[0].Description.FundingAccountID != account {
		t.Fatalf("economics = %+v (disabled keys must be absent)", economics)
	}

	// The runtime reads through these functions only.
	for _, check := range []struct{ object, privilege string }{
		{"provider_cost_events", "SELECT"}, {"provider_credentials", "SELECT"},
	} {
		var granted bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('kaana_runtime', $1, $2)`, check.object, check.privilege).Scan(&granted); err != nil || granted {
			t.Errorf("kaana_runtime %s on %s = %v/%v", check.privilege, check.object, granted, err)
		}
	}
	for _, function := range []string{
		"kaana_read_provider_attempt_feed(timestamptz, text, integer, integer)",
		"kaana_read_provider_credential_economics()",
	} {
		var granted bool
		if err := pool.QueryRow(ctx, `SELECT has_function_privilege('kaana_runtime', $1, 'EXECUTE')`, function).Scan(&granted); err != nil || !granted {
			t.Errorf("kaana_runtime cannot execute %s: %v/%v", function, granted, err)
		}
	}
}
