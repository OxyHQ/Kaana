package rotation

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestFirstOutputIsAMovingAverageThatExpires(t *testing.T) {
	clock := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	registry := NewRegistry(Policy{}, func() time.Time { return clock })

	if _, known := registry.FirstOutput("dep"); known {
		t.Fatal("a deployment nobody used was reported as observed")
	}
	permit, admitted := registry.Admit("dep")
	if !admitted {
		t.Fatal("a closed breaker refused")
	}
	permit.ObserveFirstOutput(1000 * time.Millisecond)
	permit.ObserveFirstOutput(0)
	permit.ObserveFirstOutput(-time.Second)
	if got, _ := registry.FirstOutput("dep"); got != time.Second {
		t.Fatalf("a non-positive observation changed the reading: %v", got)
	}
	permit.ObserveFirstOutput(2000 * time.Millisecond)
	if got, known := registry.FirstOutput("dep"); !known || got != 1300*time.Millisecond {
		t.Fatalf("the average of 1s then 2s at weight 0.3 is 1.3s, got %v (known %v)", got, known)
	}

	clock = clock.Add(FirstOutputWindow + time.Second)
	if _, known := registry.FirstOutput("dep"); known {
		t.Error("an observation older than the window was still evidence")
	}
	permit.ObserveFirstOutput(500 * time.Millisecond)
	if got, _ := registry.FirstOutput("dep"); got != 500*time.Millisecond {
		t.Errorf("a stale average leaked into the new reading: %v", got)
	}
}

func TestObservingLatencyNeverMovesTheBreakerOrTheScore(t *testing.T) {
	registry := NewRegistry(Policy{}, nil)
	permit, _ := registry.Admit("dep")
	permit.ObserveFirstOutput(time.Hour)
	permit.Succeeded()
	for _, entry := range registry.Project([]contract.DeploymentID{"dep"}) {
		if entry.State != StateClosed || entry.Score != 1 {
			t.Errorf("a slow first output changed health: %+v", entry)
		}
	}
	var none *Permit
	none.ObserveFirstOutput(time.Second) // a customer-credential route holds no permit
	var absent *Registry
	if _, known := absent.FirstOutput("dep"); known {
		t.Error("a nil registry reported an observation")
	}
}
