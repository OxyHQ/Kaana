package publicapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A request served while the outbox is unreachable through every one of
// Settle's own retries is still settled: the settlement is held, written once
// the outbox recovers, and delivered to Oxy exactly once.
func TestARequestServedDuringAnOutboxOutageIsSettledOnRecovery(t *testing.T) {
	metrics := &recordingMetrics{}
	h := newHarness(t, harnessOptions{metrics: metrics})
	h.outbox.setFailWrites(1 << 30)

	response := h.post("/v1/responses", validKey, responsesBody(nil))
	if body := readAll(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.settler.PendingWrites() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the settlement was not held: %d pending", h.settler.PendingWrites())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows := h.outbox.snapshot(); len(rows) != 0 {
		t.Fatalf("%d outbox rows while the outbox is down", len(rows))
	}
	if len(metrics.named("SettlementWriteDeferred")) != 1 || len(metrics.named("SettlementWriteFailed")) != 0 {
		t.Fatalf("deferred %d failed %d, want 1 and 0", len(metrics.named("SettlementWriteDeferred")), len(metrics.named("SettlementWriteFailed")))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.settler.WritePending(ctx) // still down: backs off
	if h.settler.PendingWrites() != 1 {
		t.Fatal("a failed retry dropped the held settlement")
	}

	h.outbox.setFailWrites(0)
	h.settle() // the drain writes the held settlement; delivery follows
	rows := h.outbox.snapshot()
	if len(rows) != 1 || rows[0].delivered == "" {
		t.Fatalf("rows %d (delivered %q), want one delivered row", len(rows), func() string {
			if len(rows) == 0 {
				return ""
			}
			return rows[0].delivered
		}())
	}
	if settled := h.oxy.settled(); len(settled) != 1 {
		t.Fatalf("Oxy holds %d settlements, want exactly 1", len(settled))
	}
	if len(metrics.named("SettlementWritten")) != 1 || len(metrics.named("SettlementWriteFailed")) != 0 {
		t.Fatalf("written %d failed %d, want 1 and 0", len(metrics.named("SettlementWritten")), len(metrics.named("SettlementWriteFailed")))
	}
}
