package publicapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/emf"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// outageOutbox has kaana_enqueue_settlement's semantics: one row per grant,
// the same bytes again are a replay, other bytes a conflict.
type outageOutbox struct {
	SettlementStore
	mu    sync.Mutex
	down  bool
	calls int
	rows  map[string][]byte
	// commitThenFail commits that many writes and then reports a failure,
	// as a lost COMMIT acknowledgement does.
	commitThenFail int
}

func (o *outageOutbox) RecordSettlement(_ context.Context, _ []providercost.Event, row credentialstore.OutboxSettlement) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.down {
		return errors.New("outbox unavailable")
	}
	if existing, ok := o.rows[row.GrantID]; ok {
		if bytes.Equal(existing, row.Body) {
			return nil
		}
		return credentialstore.ErrSettlementConflict
	}
	o.rows[row.GrantID] = row.Body
	if o.commitThenFail > 0 {
		o.commitThenFail--
		return errors.New("connection reset after commit")
	}
	return nil
}

func (o *outageOutbox) set(down bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.down = down
}

func (o *outageOutbox) state() (calls int, rows int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls, len(o.rows)
}

type countingMetrics struct {
	mu     sync.Mutex
	counts map[string]float64
}

func (c *countingMetrics) Record(metric emf.Metric, value float64, _ emf.Labels) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[metric.Name] += value
}

func (c *countingMetrics) count(name string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newOutageSettler(store *outageOutbox, clock *steppedClock, metrics *countingMetrics) *Settler {
	return &Settler{
		store: store, metrics: metrics, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: clock.Now, nudge: make(chan struct{}, 1),
	}
}

func heldWrite(grant string) pendingWrite {
	return pendingWrite{row: credentialstore.OutboxSettlement{
		SettlementID: "stl_" + grant, GrantID: grant, RequestID: "req_" + grant,
		Body: []byte(fmt.Sprintf(`{"settlementId":"stl_%s","grantId":"%s"}`, grant, grant)),
	}}
}

// An outbox outage far longer than Settle's own 10-second write budget: the
// held settlements are probed once per backoff, not once per settlement, the
// backoff stays capped, and on recovery each grant is written exactly once.
func TestHeldSettlementsSurviveAnOutageLongerThanTheWriteBudget(t *testing.T) {
	store := &outageOutbox{down: true, rows: map[string][]byte{}}
	clock := &steppedClock{now: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}
	metrics := &countingMetrics{counts: map[string]float64{}}
	settler := newOutageSettler(store, clock, metrics)
	for _, grant := range []string{"grt_a", "grt_b", "grt_c"} {
		if !settler.hold(heldWrite(grant)) {
			t.Fatal("the pending queue refused a settlement below its limit")
		}
	}

	ctx := context.Background()
	outage := 10 * time.Minute
	passes := 0
	for elapsed := time.Duration(0); elapsed < outage; elapsed += time.Second {
		before, _ := store.state()
		if written := settler.WritePending(ctx); written != 0 {
			t.Fatalf("%d settlements written while the outbox is down", written)
		}
		if after, _ := store.state(); after > before {
			passes++
			if after-before != 1 {
				t.Fatalf("one retry pass made %d outbox calls, want one probe", after-before)
			}
		}
		clock.advance(time.Second)
	}
	if backoff := settler.pending.retryAt.Sub(clock.Now()); backoff > settlementPendingMaxBackoff {
		t.Fatalf("backoff %v exceeds the %v ceiling", backoff, settlementPendingMaxBackoff)
	}
	// A capped backoff of 30 s over ten minutes is at least ~20 probes; an
	// uncapped one would be ~10.
	if passes < 20 {
		t.Fatalf("only %d probes in a %v outage: the backoff is not capped", passes, outage)
	}
	if held := settler.PendingWrites(); held != 3 {
		t.Fatalf("%d settlements held after the outage, want 3", held)
	}

	store.set(false)
	clock.advance(settlementPendingMaxBackoff)
	if written := settler.WritePending(ctx); written != 3 {
		t.Fatalf("%d settlements written on recovery, want 3", written)
	}
	if _, rows := store.state(); rows != 3 || settler.PendingWrites() != 0 {
		t.Fatalf("rows %d, held %d after recovery; want 3 and 0", rows, settler.PendingWrites())
	}
	if got := metrics.count("SettlementWritten"); got != 3 {
		t.Fatalf("SettlementWritten = %v, want 3", got)
	}
	if got := metrics.count("SettlementWriteFailed"); got != 0 {
		t.Fatalf("SettlementWriteFailed = %v during a recovered outage", got)
	}
	select {
	case <-settler.nudge:
	default:
		t.Fatal("delivery was not nudged after the held settlements were written")
	}
}

// A write whose COMMIT succeeded but whose answer was lost is retried with
// the same bytes: the retry is a replay, never a second row or charge.
func TestARetriedHeldSettlementIsAReplayNotASecondRow(t *testing.T) {
	store := &outageOutbox{rows: map[string][]byte{}, commitThenFail: 1}
	clock := &steppedClock{now: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}
	metrics := &countingMetrics{counts: map[string]float64{}}
	settler := newOutageSettler(store, clock, metrics)
	settler.hold(heldWrite("grt_a"))

	if written := settler.WritePending(context.Background()); written != 0 {
		t.Fatal("a write that reported failure counted as written")
	}
	clock.advance(time.Minute)
	if written := settler.WritePending(context.Background()); written != 1 {
		t.Fatal("the replay was not accepted")
	}
	if calls, rows := store.state(); calls != 2 || rows != 1 {
		t.Fatalf("calls %d rows %d, want 2 calls and exactly 1 row", calls, rows)
	}
}

// A held settlement whose grant already settled with other bytes is dropped:
// one grant settles once.
func TestAConflictingHeldSettlementIsNotWritten(t *testing.T) {
	store := &outageOutbox{rows: map[string][]byte{"grt_a": []byte(`{"other":true}`)}}
	clock := &steppedClock{now: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}
	settler := newOutageSettler(store, clock, &countingMetrics{counts: map[string]float64{}})
	settler.hold(heldWrite("grt_a"))
	settler.hold(heldWrite("grt_b"))
	if written := settler.WritePending(context.Background()); written != 1 {
		t.Fatalf("written %d, want only the non-conflicting settlement", written)
	}
	if settler.PendingWrites() != 0 {
		t.Fatal("a conflicting settlement stayed held")
	}
}

func TestThePendingQueueIsBounded(t *testing.T) {
	settler := newOutageSettler(&outageOutbox{rows: map[string][]byte{}}, &steppedClock{}, &countingMetrics{counts: map[string]float64{}})
	for index := range settlementPendingLimit {
		if !settler.hold(heldWrite(fmt.Sprintf("grt_%d", index))) {
			t.Fatalf("held %d settlements, want %d", index, settlementPendingLimit)
		}
	}
	if settler.hold(heldWrite("grt_over")) {
		t.Fatal("the pending queue grew past its limit")
	}
}

// The drain writes held settlements without waiting for the backoff; whatever
// is still held when its deadline passes is logged and counted as lost.
func TestFlushPendingWritesOnRecoveryAndCountsWhatIsLost(t *testing.T) {
	store := &outageOutbox{down: true, rows: map[string][]byte{}}
	clock := &steppedClock{now: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}
	metrics := &countingMetrics{counts: map[string]float64{}}
	settler := newOutageSettler(store, clock, metrics)
	settler.hold(heldWrite("grt_a"))
	settler.WritePending(context.Background()) // sets a backoff the flush ignores

	go func() {
		time.Sleep(3 * settlementFlushBackoff)
		store.set(false)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := settler.FlushPending(ctx); err != nil {
		t.Fatalf("flush after recovery: %v", err)
	}
	if _, rows := store.state(); rows != 1 || metrics.count("SettlementWriteFailed") != 0 {
		t.Fatalf("rows %d failed %v, want 1 and 0", rows, metrics.count("SettlementWriteFailed"))
	}

	store.set(true)
	settler.hold(heldWrite("grt_b"))
	settler.hold(heldWrite("grt_c"))
	short, cancelShort := context.WithTimeout(context.Background(), 2*settlementFlushBackoff)
	defer cancelShort()
	if err := settler.FlushPending(short); err == nil {
		t.Fatal("a flush that could not write reported success")
	}
	if got := metrics.count("SettlementWriteFailed"); got != 2 {
		t.Fatalf("SettlementWriteFailed = %v, want the 2 lost settlements", got)
	}
	if settler.PendingWrites() != 0 {
		t.Fatal("lost settlements stayed held")
	}
}
