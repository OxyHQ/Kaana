package credentialstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OxyHQ/Kaana/internal/providercost"
)

// seedRetention writes, per table, one old row and one recent row, and for the
// outbox also an OLD UNDELIVERED row, which must survive every purge.
func seedRetention(t *testing.T) (*Postgres, *pgxpool.Pool, func(table RetentionTable) int, time.Time) {
	t.Helper()
	repository, pool, ctx := openSettlementTestStore(t)
	old := time.Now().UTC().Add(-400 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-2 * 24 * time.Hour)

	for i, at := range []time.Time{old, recent} {
		event := settlementCostEvent(fmt.Sprintf("req_cost_%d", i), "key-settlement-test")
		event.OccurredAt = at
		if err := repository.RecordSettlement(ctx, []providercost.Event{event}, outboxRow(fmt.Sprintf("stl_%d", i), fmt.Sprintf("grt_%d", i), fmt.Sprintf("req_cost_%d", i))); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO provider_credential_attempt_events
			(request_id, deployment_id, credential_attempt_index, provider_slug, key_id, outcome, evidence_source, occurred_at)
			VALUES ($1, 'dep_settlement_test', 0, 'groq', 'key-settlement-test', 'accepted', 'response_status', $2)`,
			fmt.Sprintf("req_cost_%d", i), at); err != nil {
			t.Fatalf("seeding attempt event %d: %v", i, err)
		}
	}
	// stl_0 old and delivered, stl_1 recent and delivered, stl_2 old and NOT delivered.
	if err := repository.RecordSettlement(ctx, nil, outboxRow("stl_2", "grt_2", "req_undelivered")); err != nil {
		t.Fatalf("seeding the undelivered settlement: %v", err)
	}
	for _, id := range []string{"stl_0", "stl_1"} {
		if err := repository.MarkSettlementDelivered(ctx, id, "settled"); err != nil {
			t.Fatalf("delivering %s: %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE inference_settlement_outbox SET delivered_at = $1 WHERE settlement_id = 'stl_0'`, old); err != nil {
		t.Fatal(err)
	}
	// Both old rows are also OLD BY CREATION, so a purge that ignored delivery
	// state and went by age alone would take the undelivered one too.
	if _, err := pool.Exec(ctx, `UPDATE inference_settlement_outbox SET created_at = $1 WHERE settlement_id IN ('stl_0', 'stl_2')`, old); err != nil {
		t.Fatal(err)
	}
	count := func(table RetentionTable) int {
		return countRows(t, ctx, pool, "SELECT count(*) FROM "+string(table))
	}
	return repository, pool, count, time.Now().UTC().Add(-13 * 30 * 24 * time.Hour)
}

func TestARetentionDryRunCountsAndDeletesNothing(t *testing.T) {
	repository, _, count, cutoff := seedRetention(t)
	ctx := t.Context()
	for _, table := range RetentionTables() {
		before := count(table)
		eligible, err := repository.PurgeRetention(ctx, table, cutoff, 100, true)
		if err != nil {
			t.Fatalf("%s dry run: %v", table, err)
		}
		if eligible != 1 {
			t.Errorf("%s: %d eligible, want exactly the one old row", table, eligible)
		}
		if count(table) != before {
			t.Errorf("%s: a dry run changed the table", table)
		}
	}
}

func TestARetentionPurgeDeletesOnlyOldRowsAndNeverAnUndeliveredSettlement(t *testing.T) {
	repository, pool, count, cutoff := seedRetention(t)
	ctx := t.Context()
	for _, table := range RetentionTables() {
		deleted, err := repository.PurgeRetention(ctx, table, cutoff, 100, false)
		if err != nil || deleted != 1 {
			t.Fatalf("%s purge deleted %d: %v", table, deleted, err)
		}
	}
	if got := count(RetentionCostEvents); got != 1 {
		t.Errorf("cost events left = %d, want the recent one", got)
	}
	if got := count(RetentionAttemptEvents); got != 1 {
		t.Errorf("attempt events left = %d, want the recent one", got)
	}
	// stl_1 (recent, delivered) and stl_2 (old, UNDELIVERED) stay.
	if got := count(RetentionSettlementsOut); got != 2 {
		t.Errorf("outbox rows left = %d, want the recent delivered one and the old undelivered one", got)
	}
	for id, want := range map[string]int{"stl_0": 0, "stl_1": 1, "stl_2": 1} {
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM inference_settlement_outbox WHERE settlement_id = $1`, id); got != want {
			t.Errorf("settlement %s present = %d, want %d (stl_0 old+delivered goes, stl_1 recent stays, stl_2 undelivered stays)", id, got, want)
		}
	}
	again, err := repository.PurgeRetention(ctx, RetentionSettlementsOut, cutoff, 100, false)
	if err != nil || again != 0 {
		t.Errorf("a second purge deleted %d (%v), want 0: the undelivered settlement is never purgeable", again, err)
	}
}

func TestARetentionPurgeIsBoundedByItsBatch(t *testing.T) {
	repository, _, ctx := openSettlementTestStore(t)
	old := time.Now().UTC().Add(-400 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		event := settlementCostEvent(fmt.Sprintf("req_batch_%d", i), "key-settlement-test")
		event.OccurredAt = old
		if err := repository.RecordSettlement(ctx, []providercost.Event{event}, outboxRow(fmt.Sprintf("stl_b%d", i), fmt.Sprintf("grt_b%d", i), fmt.Sprintf("req_batch_%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().UTC().Add(-100 * 24 * time.Hour)
	first, err := repository.PurgeRetention(ctx, RetentionCostEvents, cutoff, 2, false)
	if err != nil || first != 2 {
		t.Fatalf("first batch deleted %d (%v), want 2", first, err)
	}
	total := first
	for {
		n, err := repository.PurgeRetention(ctx, RetentionCostEvents, cutoff, 2, false)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		if n < 2 {
			break
		}
	}
	if total != 5 {
		t.Errorf("batches deleted %d rows in total, want 5", total)
	}
}

func TestARetentionCutoffNewerThanThirtyDaysIsRefusedByTheDatabase(t *testing.T) {
	repository, _, _, _ := seedRetention(t)
	ctx := t.Context()
	for _, age := range []time.Duration{0, 24 * time.Hour, 29 * 24 * time.Hour} {
		for _, table := range RetentionTables() {
			if _, err := repository.PurgeRetention(ctx, table, time.Now().Add(-age), 10, false); err == nil {
				t.Errorf("%s: a cutoff %s old was accepted", table, age)
			}
		}
	}
	if _, err := repository.PurgeRetention(ctx, "users", time.Now().Add(-400*24*time.Hour), 10, true); err == nil {
		t.Error("an arbitrary table name was accepted")
	}
	if _, err := repository.PurgeRetention(ctx, RetentionCostEvents, time.Now().Add(-400*24*time.Hour), MaxPurgeBatch+1, true); err == nil {
		t.Error("an oversized batch was accepted")
	}
}

func TestOnlyTheCredentialAdminMayPurge(t *testing.T) {
	_, pool, ctx := openSettlementTestStore(t)
	for _, function := range []string{
		"kaana_purge_provider_cost_events(timestamptz,integer,boolean)",
		"kaana_purge_provider_credential_attempt_events(timestamptz,integer,boolean)",
		"kaana_purge_delivered_settlements(timestamptz,integer,boolean)",
	} {
		for role, want := range map[string]bool{"kaana_credential_admin": true, "kaana_runtime": false, "kaana_customer_credential_control": false, "kaana_platform_credential_control": false} {
			var has bool
			if err := pool.QueryRow(ctx, `SELECT has_function_privilege($1, $2, 'EXECUTE')`, role, function).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("%s may execute %s = %v, want %v", role, function, has, want)
			}
		}
	}
}
