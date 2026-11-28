package publicapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/emf"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// ErrSettlementWriteDeferred is Settle's answer when the outbox refused the
// write and the settlement is held in memory: the settler's worker retries the
// same bytes until the outbox accepts them, and the drain flushes them before
// the task stops.
var ErrSettlementWriteDeferred = errors.New("publicapi: the settlement write is deferred until the outbox recovers")

// Pending-write bounds. A held settlement is a few kilobytes, so the limit
// caps memory near tens of megabytes. Retries back off to a ceiling that is
// short against Oxy's hold lifetime.
const (
	settlementPendingLimit      = 4096
	settlementPendingMaxBackoff = 30 * time.Second
	settlementFlushBackoff      = 250 * time.Millisecond
)

// pendingWrite is a settlement the outbox has not accepted. It keeps the exact
// row and events of the first attempt. A retry of the same bytes is a replay
// in kaana_enqueue_settlement, so a write that committed although its answer
// was lost never becomes a second settlement or a second charge.
type pendingWrite struct {
	events []providercost.Event
	row    credentialstore.OutboxSettlement
}

// pendingWrites is the bounded in-memory queue of settlements awaiting the
// outbox, retried oldest first with one backoff for the whole queue: while the
// database is down, one probe per interval, not one per held settlement.
type pendingWrites struct {
	mu       sync.Mutex
	items    []pendingWrite
	failures int
	retryAt  time.Time

	// writing serializes the worker's retry pass and the drain's flush.
	writing sync.Mutex
}

// hold queues a settlement the outbox refused. It reports false when the
// queue is full, and the caller then records the settlement as lost.
func (s *Settler) hold(write pendingWrite) bool {
	s.pending.mu.Lock()
	defer s.pending.mu.Unlock()
	if len(s.pending.items) >= settlementPendingLimit {
		return false
	}
	s.pending.items = append(s.pending.items, write)
	return true
}

// PendingWrites is how many settlements are waiting for the outbox.
func (s *Settler) PendingWrites() int {
	s.pending.mu.Lock()
	defer s.pending.mu.Unlock()
	return len(s.pending.items)
}

// WritePending retries the held settlements once their backoff has passed,
// oldest first, and stops at the first failure: the outbox is still down.
// It reports how many reached the outbox.
func (s *Settler) WritePending(ctx context.Context) int {
	return s.writePending(ctx, false)
}

func (s *Settler) writePending(ctx context.Context, ignoreBackoff bool) int {
	s.pending.writing.Lock()
	defer s.pending.writing.Unlock()
	written := 0
	for ctx.Err() == nil {
		s.pending.mu.Lock()
		if len(s.pending.items) == 0 || (!ignoreBackoff && s.now().Before(s.pending.retryAt)) {
			s.pending.mu.Unlock()
			break
		}
		write := s.pending.items[0]
		s.pending.mu.Unlock()

		writeContext, cancel := context.WithTimeout(ctx, settlementWriteTimeout)
		err := s.store.RecordSettlement(writeContext, write.events, write.row)
		cancel()

		s.pending.mu.Lock()
		if err != nil && !errors.Is(err, credentialstore.ErrSettlementConflict) {
			s.pending.failures++
			backoff := min(settlementPendingMaxBackoff, time.Second<<min(s.pending.failures-1, 5))
			s.pending.retryAt = s.now().Add(backoff)
			held := len(s.pending.items)
			s.pending.mu.Unlock()
			s.logger.Warn("held settlements still cannot be written to the outbox; retrying",
				"held", held, "retryInMs", backoff.Milliseconds(), "errorType", "settlement_persistence", "error", err)
			return written
		}
		s.pending.items = s.pending.items[1:]
		s.pending.failures = 0
		s.pending.retryAt = time.Time{}
		s.pending.mu.Unlock()
		if err != nil {
			// The grant already settled with other bytes. One grant settles
			// once, so this settlement must not be written.
			s.logger.Error("a held settlement conflicts with the outbox and was not written",
				"requestId", write.row.RequestID, "grantId", write.row.GrantID, "errorType", "settlement_conflict")
			continue
		}
		written++
		s.record(emf.SettlementWritten, 1)
		s.logger.Info("a held settlement reached the outbox", "requestId", write.row.RequestID, "grantId", write.row.GrantID)
	}
	if written > 0 {
		select {
		case s.nudge <- struct{}{}:
		default:
		}
	}
	return written
}

// FlushPending writes every held settlement before the task stops, retrying
// without the worker's backoff until ctx ends. Whatever is still held then is
// lost: it is logged and counted as SettlementWriteFailed.
func (s *Settler) FlushPending(ctx context.Context) error {
	for {
		s.writePending(ctx, true)
		held := s.PendingWrites()
		if held == 0 {
			return nil
		}
		timer := time.NewTimer(settlementFlushBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.pending.writing.Lock()
			defer s.pending.writing.Unlock()
			s.pending.mu.Lock()
			lost := s.pending.items
			s.pending.items = nil
			s.pending.mu.Unlock()
			for _, write := range lost {
				s.logger.Error("a held settlement could not be written before the task stopped and is lost",
					"requestId", write.row.RequestID, "grantId", write.row.GrantID, "errorType", "settlement_persistence")
			}
			s.record(emf.SettlementWriteFailed, float64(len(lost)))
			return fmt.Errorf("publicapi: %d settlements could not be written before shutdown: %w", len(lost), ctx.Err())
		case <-timer.C:
		}
	}
}
