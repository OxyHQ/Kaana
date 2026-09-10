package credentialstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProviderCostEventsAreExactlyIdempotentInPostgres(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("opening test PostgreSQL: %v", err)
	}
	defer pool.Close()
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE
		provider_cost_events,
		provider_credential_admin_operations,
		provider_credential_audit,
		provider_credentials RESTART IDENTITY`); err != nil {
		t.Fatalf("resetting provider cost test tables: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials
		(provider_slug, key_id, encrypted_secret, kms_key_arn, position)
		VALUES ('groq', 'key-cost-test', '\x01',
		'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', 1)`); err != nil {
		t.Fatalf("creating cost event key identity: %v", err)
	}

	at := time.Date(2026, time.September, 11, 12, 0, 0, 123_000, time.UTC)
	event := providercost.Event{
		RequestID: "req_cost_integration", AttemptIndex: 0,
		Provider: "groq", KeyID: "key-cost-test", DeploymentID: "dep_cost_test",
		ModelReference: "openai/gpt-oss-120b@2026-08-01",
		Cost:           providercost.Money{Currency: "USD", Amount: 125_000},
		Source:         providercost.SourceRateCard, RateCardVersionID: "rc_integration_v1",
		Complete: true, Served: true, OccurredAt: at,
	}
	if err := repository.WriteProviderCostEvent(ctx, event); err != nil {
		t.Fatalf("WriteProviderCostEvent: %v", err)
	}
	if err := repository.WriteProviderCostEvent(ctx, event); err != nil {
		t.Fatalf("idempotent WriteProviderCostEvent: %v", err)
	}
	conflict := event
	conflict.Served = false
	if err := repository.WriteProviderCostEvent(ctx, conflict); err == nil {
		t.Fatal("a reused request-attempt identity accepted different facts")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM provider_cost_events
		WHERE request_id = 'req_cost_integration' AND attempt_index = 0`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("persisted event rows/error = %d/%v, want 1/nil", rows, err)
	}

	unknown := event
	unknown.AttemptIndex = 1
	unknown.Cost = providercost.Money{}
	unknown.Source = providercost.SourceUnknown
	unknown.RateCardVersionID = ""
	unknown.Complete = false
	if err := repository.WriteProviderCostEvent(ctx, unknown); err != nil {
		t.Fatalf("recording an explicitly unknown cost: %v", err)
	}
	var currency *string
	var amount *int64
	if err := pool.QueryRow(ctx, `SELECT currency, amount_picos FROM provider_cost_events
		WHERE request_id = 'req_cost_integration' AND attempt_index = 1`).Scan(&currency, &amount); err != nil {
		t.Fatalf("reading unknown cost: %v", err)
	}
	if currency != nil || amount != nil {
		t.Fatalf("unknown cost persisted as currency/amount %v/%v", currency, amount)
	}

	atomicExisting := event
	atomicExisting.RequestID = "req_cost_atomic"
	atomicExisting.AttemptIndex = 1
	if err := repository.WriteProviderCostEvent(ctx, atomicExisting); err != nil {
		t.Fatalf("seeding atomic conflict: %v", err)
	}
	atomicFirst := event
	atomicFirst.RequestID = "req_cost_atomic"
	atomicFirst.AttemptIndex = 0
	atomicConflict := atomicExisting
	atomicConflict.Served = false
	if err := repository.WriteProviderCostEvents(ctx, []providercost.Event{atomicFirst, atomicConflict}); err == nil {
		t.Fatal("batch with a conflicting later attempt succeeded")
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM provider_cost_events
		WHERE request_id = 'req_cost_atomic' AND attempt_index = 0`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed batch partially persisted its first event: rows/error=%d/%v", rows, err)
	}
	if err := repository.WriteProviderCostEvents(ctx, []providercost.Event{event, unknown}); err != nil {
		t.Fatalf("idempotent full batch replay: %v", err)
	}
}
