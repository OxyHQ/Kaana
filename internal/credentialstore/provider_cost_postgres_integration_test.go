package credentialstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
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
		provider_credentials RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("resetting provider cost test tables: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials
		(provider_slug, key_id, encrypted_secret, kms_key_arn, position)
		VALUES ('groq', 'key-cost-test', '\x01',
		'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', 1)`); err != nil {
		t.Fatalf("creating cost event key identity: %v", err)
	}

	at := time.Date(2026, time.September, 11, 12, 0, 0, 123_000, time.UTC)
	// Rate-card versions are append-only, so each run registers its own.
	rateCardVersion := fmt.Sprintf("rc_integration_%d", time.Now().UnixNano())
	unregistered := providercost.Event{
		RequestID: "req_cost_unregistered", Provider: "groq", KeyID: "key-cost-test", DeploymentID: "dep_cost_test",
		ModelReference: "openai/gpt-oss-120b@2026-08-01", Cost: providercost.Money{Currency: "USD", Amount: 1},
		Source: providercost.SourceRateCard, RateCardVersionID: rateCardVersion, Complete: true, OccurredAt: at,
		Units:     []contract.UsageQuantity{},
		Telemetry: providercost.AttemptTelemetry{StartedAt: at, Outcome: providercost.AttemptSucceeded},
	}
	if err := repository.WriteProviderCostEvent(ctx, unregistered); err == nil {
		t.Fatal("a rate-card cost naming an unregistered version was recorded")
	}
	cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"` + rateCardVersion + `","source":"provider_documentation",
		"sourceVersion":"groq-pricing-2026-09-01","observedAt":"2026-09-01T00:00:00Z","effectiveAt":"2026-09-01T00:00:00Z",
		"rateCards":[{"deploymentId":"dep_cost_test","currency":"USD","rates":[{"unit":"output_tokens","amountPerUnit":10}]}]}`))
	if err != nil {
		t.Fatalf("parsing rate card: %v", err)
	}
	observation, _ := cards.Observation()
	for range 2 {
		if err := repository.RegisterRateCardVersion(ctx, observation); err != nil {
			t.Fatalf("registering (and replaying) the loaded rate card version: %v", err)
		}
	}
	repriced := observation
	repriced.RateCards = []byte(`[{"deploymentId":"dep_cost_test","currency":"USD","rates":[{"unit":"output_tokens","amountPerUnit":11}]}]`)
	if err := repository.RegisterRateCardVersion(ctx, repriced); err == nil {
		t.Fatal("a different price was accepted under an existing rate card version")
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_rate_card_versions SET source_version = 'rewritten' WHERE version_id = $1`, rateCardVersion); err == nil {
		t.Fatal("a registered rate card version was rewritten in place")
	}

	event := providercost.Event{
		RequestID: "req_cost_integration", AttemptIndex: 0,
		Provider: "groq", KeyID: "key-cost-test", DeploymentID: "dep_cost_test",
		ModelReference: "openai/gpt-oss-120b@2026-08-01",
		Cost:           providercost.Money{Currency: "USD", Amount: 125_000},
		Source:         providercost.SourceRateCard, RateCardVersionID: rateCardVersion,
		Complete: true, Served: true, OccurredAt: at,
		Units: []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 12}, {Unit: contract.UnitOutputTokens, Quantity: 3}},
		Telemetry: providercost.AttemptTelemetry{
			StartedAt: at.Add(-900 * time.Millisecond), Latency: 900 * time.Millisecond,
			TimeToFirstOutput: 250 * time.Millisecond, Outcome: providercost.AttemptSucceeded,
		},
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

	for name, mutate := range map[string]func(*providercost.Event){
		"latency":      func(e *providercost.Event) { e.Telemetry.Latency += time.Millisecond },
		"first output": func(e *providercost.Event) { e.Telemetry.TimeToFirstOutput = 0 },
		"units":        func(e *providercost.Event) { e.Units = e.Units[:1] },
		"outcome": func(e *providercost.Event) {
			e.Telemetry.Outcome, e.Telemetry.FailureCode = providercost.AttemptFailed, contract.CodeRateLimited
		},
	} {
		conflict := event
		conflict.Units = append([]contract.UsageQuantity{}, event.Units...)
		mutate(&conflict)
		if err := repository.WriteProviderCostEvent(ctx, conflict); err == nil {
			t.Errorf("a replay with a different %s was accepted as the same attempt", name)
		}
	}
	var (
		storedUnits   []contract.UsageQuantity
		latency, ttft *int64
		outcome       *string
		failureCode   *string
	)
	if err := pool.QueryRow(ctx, `SELECT usage_units, latency_ms, time_to_first_output_ms, attempt_outcome, failure_code
		FROM provider_cost_events WHERE request_id = 'req_cost_integration' AND attempt_index = 0`).
		Scan(&storedUnits, &latency, &ttft, &outcome, &failureCode); err != nil {
		t.Fatalf("reading attempt telemetry: %v", err)
	}
	if len(storedUnits) != 2 || latency == nil || *latency != 900 || ttft == nil || *ttft != 250 ||
		outcome == nil || *outcome != "succeeded" || failureCode != nil {
		t.Fatalf("stored telemetry = units %v latency %v ttft %v outcome %v failure %v", storedUnits, latency, ttft, outcome, failureCode)
	}

	throttled := event
	throttled.RequestID = "req_cost_throttled"
	throttled.Served = false
	throttled.Telemetry.TimeToFirstOutput = 0
	throttled.Telemetry.Outcome = providercost.AttemptFailed
	throttled.Telemetry.FailureCode = contract.CodeRateLimited
	if err := repository.WriteProviderCostEvent(ctx, throttled); err != nil {
		t.Fatalf("recording a throttled attempt: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT failure_code, time_to_first_output_ms FROM provider_cost_events
		WHERE request_id = 'req_cost_throttled'`).Scan(&failureCode, &ttft); err != nil {
		t.Fatalf("reading throttled attempt: %v", err)
	}
	if failureCode == nil || *failureCode != "rate_limited" || ttft != nil {
		t.Fatalf("throttled attempt stored failure %v first output %v", failureCode, ttft)
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
