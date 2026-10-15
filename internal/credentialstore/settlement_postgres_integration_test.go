package credentialstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

func openSettlementTestStore(t *testing.T) (*Postgres, *pgxpool.Pool, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("opening test PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE inference_settlement_outbox, provider_cost_events,
		provider_credential_admin_operations, provider_credential_audit, provider_credentials RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("resetting settlement test tables: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials
		(provider_slug, key_id, encrypted_secret, kms_key_arn, position)
		VALUES ('groq', 'key-settlement-test', '\x01',
		'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', 1)`); err != nil {
		t.Fatalf("creating the settlement key identity: %v", err)
	}
	return repository, pool, ctx
}

func settlementCostEvent(requestID string, keyID string) providercost.Event {
	at := time.Date(2026, time.October, 15, 10, 0, 0, 0, time.UTC)
	return providercost.Event{
		RequestID: contract.RequestID(requestID), AttemptIndex: 0, Provider: "groq", KeyID: keyID,
		DeploymentID: "dep_settlement_test", ModelReference: "openai/gpt-oss-120b@2026-08-01",
		Source: providercost.SourceUnknown, Served: true, OccurredAt: at,
		Units: []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 3}},
		Telemetry: providercost.AttemptTelemetry{
			StartedAt: at.Add(-time.Second), Latency: time.Second, Outcome: providercost.AttemptSucceeded,
		},
	}
}

func outboxRow(settlementID, grantID, requestID string) OutboxSettlement {
	return OutboxSettlement{SettlementID: settlementID, GrantID: grantID, RequestID: requestID,
		Body: []byte(`{"schemaVersion":1,"settlementId":"` + settlementID + `","grantId":"` + grantID + `","requestId":"` + requestID + `"}`)}
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

// TestSettlementAndCostAreOneTransaction is the outbox's central promise: a
// settlement is durable exactly when its provider costs are, and one grant
// settles once however often the write is retried.
func TestSettlementAndCostAreOneTransaction(t *testing.T) {
	repository, pool, ctx := openSettlementTestStore(t)

	row := outboxRow("stl_a", "grt_a", "req_a")
	event := settlementCostEvent("req_a", "key-settlement-test")
	if err := repository.RecordSettlement(ctx, []providercost.Event{event}, row); err != nil {
		t.Fatalf("RecordSettlement: %v", err)
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM inference_settlement_outbox WHERE grant_id = 'grt_a'`); got != 1 {
		t.Fatalf("outbox rows = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM provider_cost_events WHERE request_id = 'req_a'`); got != 1 {
		t.Fatalf("cost rows = %d, want 1", got)
	}

	// A retried transaction is a replay.
	if err := repository.RecordSettlement(ctx, []providercost.Event{event}, row); err != nil {
		t.Fatalf("replayed RecordSettlement: %v", err)
	}
	// The same grant with other bytes is a conflict, never a second settlement.
	other := outboxRow("stl_b", "grt_a", "req_a")
	if err := repository.RecordSettlement(ctx, nil, other); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("a second settlement of one grant = %v, want ErrSettlementConflict", err)
	}
	changed := row
	changed.Body = []byte(`{"schemaVersion":1,"settlementId":"stl_a","grantId":"grt_a","requestId":"req_a","servedStep":0}`)
	if err := repository.RecordSettlement(ctx, nil, changed); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("a settlement rewritten under its own id = %v, want ErrSettlementConflict", err)
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM inference_settlement_outbox`); got != 1 {
		t.Fatalf("outbox rows after conflicts = %d, want 1", got)
	}

	// A cost that cannot be recorded takes its settlement down with it: an
	// unknown key fails the cost batch, and the settlement must not survive.
	broken := outboxRow("stl_c", "grt_c", "req_c")
	if err := repository.RecordSettlement(ctx, []providercost.Event{settlementCostEvent("req_c", "key-nobody-has")}, broken); err == nil {
		t.Fatal("a settlement whose cost failed to record was accepted")
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM inference_settlement_outbox WHERE grant_id = 'grt_c'`); got != 0 {
		t.Fatalf("a settlement survived its cost's failure (%d rows)", got)
	}

	// The body's identities must be the row's.
	lying := outboxRow("stl_d", "grt_d", "req_d")
	lying.GrantID = "grt_other"
	if err := repository.RecordSettlement(ctx, nil, lying); err == nil {
		t.Fatal("a settlement whose body names another grant was accepted")
	}

	// Positive control for a cost-free settlement (nothing was attempted).
	if err := repository.RecordSettlement(ctx, nil, outboxRow("stl_e", "grt_e", "req_e")); err != nil {
		t.Fatalf("a settlement without attempts: %v", err)
	}
}

// TestSettlementDeliveryLeasesAndAcknowledges drives the worker's half: due
// rows are leased to one deliverer, a deferral delays a row, an
// acknowledgement retires it, and the reconciliation feed reads every row only
// once it is safely committed.
func TestSettlementDeliveryLeasesAndAcknowledges(t *testing.T) {
	repository, pool, ctx := openSettlementTestStore(t)
	for _, id := range []string{"1", "2", "3"} {
		if err := repository.RecordSettlement(ctx, nil, outboxRow("stl_"+id, "grt_"+id, "req_"+id)); err != nil {
			t.Fatalf("RecordSettlement %s: %v", id, err)
		}
	}

	first, err := repository.ClaimSettlements(ctx, 2, time.Minute)
	if err != nil || len(first) != 2 || first[0].SettlementID != "stl_1" || first[0].Attempts != 1 {
		t.Fatalf("first claim = %+v, %v; want stl_1 and stl_2 on their first attempt", first, err)
	}
	second, err := repository.ClaimSettlements(ctx, 10, time.Minute)
	if err != nil || len(second) != 1 || second[0].SettlementID != "stl_3" {
		t.Fatalf("second claim = %+v, %v; leased rows must not be claimed again", second, err)
	}
	if again, err := repository.ClaimSettlements(ctx, 10, time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("a third claim found %d rows (%v) while all were leased", len(again), err)
	}

	if err := repository.MarkSettlementDelivered(ctx, "stl_1", "settled"); err != nil {
		t.Fatalf("MarkSettlementDelivered: %v", err)
	}
	if err := repository.MarkSettlementDelivered(ctx, "stl_1", "duplicate"); err != nil {
		t.Fatalf("a repeated acknowledgement: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT delivered_status FROM inference_settlement_outbox WHERE settlement_id = 'stl_1'`).Scan(&status); err != nil || status != "settled" {
		t.Fatalf("delivered status = %q/%v; the first acknowledgement stands", status, err)
	}
	if err := repository.MarkSettlementDelivered(ctx, "stl_2", "maybe"); err == nil {
		t.Fatal("an acknowledgement outside the contract's vocabulary was recorded")
	}

	if err := repository.DeferSettlement(ctx, "stl_2", time.Now().Add(-time.Second), "oxy_unavailable"); err != nil {
		t.Fatalf("DeferSettlement: %v", err)
	}
	if err := repository.DeferSettlement(ctx, "stl_3", time.Now().Add(time.Hour), "Oxy said: no"); err == nil {
		t.Fatal("free text was recorded as a delivery failure")
	}
	due, err := repository.ClaimSettlements(ctx, 10, time.Minute)
	if err != nil || len(due) != 1 || due[0].SettlementID != "stl_2" || due[0].Attempts != 2 {
		t.Fatalf("after deferral the due rows are %+v (%v); want stl_2 on its second attempt", due, err)
	}

	// The reconciliation feed hides rows younger than its settle window.
	if page, err := repository.ReadSettlements(ctx, 0, 10); err != nil || len(page) != 0 {
		t.Fatalf("fresh rows were visible to reconciliation: %d (%v)", len(page), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE inference_settlement_outbox SET created_at = created_at - INTERVAL '1 minute'`); err != nil {
		t.Fatal(err)
	}
	page, err := repository.ReadSettlements(ctx, 0, 2)
	if err != nil || len(page) != 2 || page[0].Position >= page[1].Position {
		t.Fatalf("reconciliation page = %+v (%v)", page, err)
	}
	rest, err := repository.ReadSettlements(ctx, page[1].Position, 10)
	if err != nil || len(rest) != 1 || string(rest[0].Body) != string(outboxRow("stl_3", "grt_3", "req_3").Body) {
		t.Fatalf("the next page = %+v (%v)", rest, err)
	}
	if _, err := repository.ReadSettlements(ctx, -1, 10); err == nil {
		t.Fatal("a negative cursor was accepted")
	}
}
