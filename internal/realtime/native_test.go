package realtime_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/realtime"
)

type nativeGate struct {
	deadline  time.Time
	finalized atomic.Int32
	failure   bool
}

func (g *nativeGate) Credential(r *http.Request) ([32]byte, bool) {
	return sha256.Sum256([]byte(r.Header.Get("Authorization"))), true
}
func (g *nativeGate) OriginPatterns() []string { return nil }
func (g *nativeGate) Admit(_ context.Context, r *http.Request, _ []byte) (*realtime.PublicAdmission, *contract.PublicError) {
	request := sessionRequest("native-deadline", primary)
	digest, _ := g.Credential(r)
	return &realtime.PublicAdmission{Request: &request, Credential: digest, ExecutionDeadline: g.deadline, Finalize: func(context.Context, kaana.SessionSettlement, *kaana.SessionOpening) error {
		g.finalized.Add(1)
		if g.failure {
			return errors.New("durability unavailable")
		}
		return nil
	}}, nil
}
func TestNativeRealtimeUsesAbsoluteAdmissionDeadline(t *testing.T) {
	gate := &nativeGate{}
	h := newHarness(t, options{gate: gate})
	// Admission starts after harness setup; retain the same absolute one-second lease.
	gate.deadline = time.Now().Add(time.Second)
	ws := publicConnect(t, h, "Bearer synthetic", []byte(`{"type":"session.open"}`))
	created := read(t, ws)
	var event contract.RealtimePublicSessionCreatedEvent
	if err := json.Unmarshal(encode(t, created), &event); err != nil {
		t.Fatal(err)
	}
	expiresAt, err := event.ExpiresAt.Time()
	if err != nil || expiresAt.Sub(gate.deadline) > time.Millisecond {
		t.Fatal("opening extended admitted execution lease")
	}
	for {
		next := read(t, ws)
		if next.kind() == "session.closed" {
			var closed contract.RealtimePublicSessionClosedEvent
			if err := json.Unmarshal(encode(t, next), &closed); err != nil || closed.Reason != contract.RealtimeMaxDuration {
				t.Fatalf("wrong terminal: %s", encode(t, next))
			}
			break
		}
	}
	if gate.finalized.Load() != 1 {
		t.Fatal("closed announced before durable completion")
	}
}
