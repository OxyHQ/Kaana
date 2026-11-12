package deploymentstats

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newClock() *clock { return &clock{at: time.Unix(1_800_000_000, 0)} }

const dep = contract.DeploymentID("dep_a")

func window(t *testing.T, r *Registry, span time.Duration) Window {
	t.Helper()
	snapshot := r.Project([]contract.DeploymentID{dep})
	if len(snapshot) != 1 {
		t.Fatalf("projection has %d entries, want 1", len(snapshot))
	}
	for _, w := range snapshot[0].Windows {
		if w.Seconds == int(span/time.Second) {
			return w
		}
	}
	t.Fatalf("no %v window", span)
	return Window{}
}

func record(r *Registry, n int, outcome Outcome) {
	for range n {
		r.Record(dep, Attempt{Outcome: outcome})
	}
}

func TestUnseenDeploymentReportsEmptyWindowsNotHealth(t *testing.T) {
	r := NewRegistry(newClock().now)
	w := window(t, r, 5*time.Minute)
	if w.Attempts != 0 || w.Uptime != nil || w.TimeToFirstOutputMs != nil || w.TokensPerSecond != nil {
		t.Fatalf("an unseen deployment must report nothing, got %+v", w)
	}
}

func TestCountsAreSplitByOutcome(t *testing.T) {
	r := NewRegistry(newClock().now)
	record(r, 7, Succeeded)
	record(r, 2, Throttled)
	record(r, 1, Failed)
	w := window(t, r, 5*time.Minute)
	if w.Attempts != 10 || w.Succeeded != 7 || w.Throttled != 2 {
		t.Fatalf("counts = %+v", w)
	}
}

func TestUptimeNeedsEnoughAttemptsToBeObserved(t *testing.T) {
	c := newClock()
	r := NewRegistry(c.now)
	// One failed request in an otherwise idle minute is not an outage.
	record(r, 1, Failed)
	if w := window(t, r, 5*time.Minute); w.Uptime != nil {
		t.Fatalf("a minute under %d attempts must not be observed, got uptime %v", MinMinuteAttempts, *w.Uptime)
	}
}

func TestUptimeIsUpMinutesOverObservedMinutes(t *testing.T) {
	c := newClock()
	r := NewRegistry(c.now)
	record(r, 10, Succeeded) // up: 100%
	c.at = c.at.Add(time.Minute)
	record(r, 8, Succeeded)
	record(r, 2, Failed) // up: exactly 80%
	c.at = c.at.Add(time.Minute)
	record(r, 7, Succeeded)
	record(r, 3, Failed) // down: 70%
	c.at = c.at.Add(time.Minute)
	record(r, 3, Failed) // not observed

	w := window(t, r, 5*time.Minute)
	if w.Uptime == nil {
		t.Fatal("uptime is absent")
	}
	if got, want := *w.Uptime, 2.0/3.0; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("uptime = %v, want %v (two up of three observed minutes)", got, want)
	}
}

func TestOldMinutesLeaveTheShortWindowButStayInTheDay(t *testing.T) {
	c := newClock()
	r := NewRegistry(c.now)
	record(r, 10, Succeeded)
	c.at = c.at.Add(10 * time.Minute)
	record(r, 4, Succeeded)

	if w := window(t, r, 5*time.Minute); w.Attempts != 4 {
		t.Fatalf("5m window attempts = %d, want 4", w.Attempts)
	}
	if w := window(t, r, 30*time.Minute); w.Attempts != 14 {
		t.Fatalf("30m window attempts = %d, want 14", w.Attempts)
	}
	if w := window(t, r, 24*time.Hour); w.Attempts != 14 {
		t.Fatalf("1d window attempts = %d, want 14", w.Attempts)
	}
}

func TestAMinuteBucketIsReusedAfterADay(t *testing.T) {
	c := newClock()
	r := NewRegistry(c.now)
	record(r, 5, Succeeded)
	c.at = c.at.Add(24 * time.Hour)
	record(r, 3, Succeeded)
	if w := window(t, r, 24*time.Hour); w.Attempts != 3 {
		t.Fatalf("a day-old minute must be replaced, not summed: attempts = %d, want 3", w.Attempts)
	}
}

func TestLatencyAndThroughputPercentiles(t *testing.T) {
	r := NewRegistry(newClock().now)
	for i := 1; i <= 100; i++ {
		r.Record(dep, Attempt{
			Outcome:           Succeeded,
			TimeToFirstOutput: time.Duration(i) * time.Millisecond,
			Latency:           time.Duration(i)*time.Millisecond + time.Second,
			OutputTokens:      50,
		})
	}
	w := window(t, r, 5*time.Minute)
	if w.TimeToFirstOutputMs == nil || w.TimeToFirstOutputMs.P50 != 50 || w.TimeToFirstOutputMs.P90 != 90 || w.TimeToFirstOutputMs.P99 != 99 {
		t.Fatalf("ttft percentiles = %+v", w.TimeToFirstOutputMs)
	}
	// 50 tokens over one second of generation, whatever the first-output delay.
	if w.TokensPerSecond == nil || w.TokensPerSecond.P50 != 50 {
		t.Fatalf("throughput = %+v, want 50 tokens per second", w.TokensPerSecond)
	}
}

func TestFailuresAndSilentAttemptsContributeNoSamples(t *testing.T) {
	r := NewRegistry(newClock().now)
	r.Record(dep, Attempt{Outcome: Failed, TimeToFirstOutput: time.Second, Latency: 2 * time.Second, OutputTokens: 10})
	r.Record(dep, Attempt{Outcome: Succeeded}) // nothing was ever delivered
	w := window(t, r, 5*time.Minute)
	if w.TimeToFirstOutputMs != nil || w.TokensPerSecond != nil {
		t.Fatalf("no sample expected, got %+v %+v", w.TimeToFirstOutputMs, w.TokensPerSecond)
	}
}

func TestThroughputIsAbsentWhenGenerationTimeIsUnknown(t *testing.T) {
	r := NewRegistry(newClock().now)
	r.Record(dep, Attempt{Outcome: Succeeded, TimeToFirstOutput: time.Second, Latency: time.Second, OutputTokens: 10})
	r.Record(dep, Attempt{Outcome: Succeeded, TimeToFirstOutput: time.Second, Latency: 2 * time.Second})
	w := window(t, r, 5*time.Minute)
	if w.TimeToFirstOutputMs == nil {
		t.Fatal("ttft should still be reported")
	}
	if w.TokensPerSecond != nil {
		t.Fatalf("throughput must not be invented: %+v", w.TokensPerSecond)
	}
}

func TestSamplesAreBoundedAndOldOnesFallOut(t *testing.T) {
	c := newClock()
	r := NewRegistry(c.now)
	for range samplesKept + 100 {
		r.Record(dep, Attempt{Outcome: Succeeded, TimeToFirstOutput: time.Millisecond, Latency: 2 * time.Millisecond})
	}
	r.mu.Lock()
	kept := len(r.series[dep].samples)
	r.mu.Unlock()
	if kept != samplesKept {
		t.Fatalf("kept %d samples, want the bound %d", kept, samplesKept)
	}
	c.at = c.at.Add(time.Hour)
	if w := window(t, r, 30*time.Minute); w.TimeToFirstOutputMs != nil {
		t.Fatalf("samples older than the window must not count: %+v", w.TimeToFirstOutputMs)
	}
}

func TestRetainDropsDeploymentsOutsideTheInventory(t *testing.T) {
	r := NewRegistry(newClock().now)
	record(r, 3, Succeeded)
	r.Retain([]contract.DeploymentID{"dep_other"})
	if w := window(t, r, 5*time.Minute); w.Attempts != 0 {
		t.Fatalf("retained a deployment that left the inventory: %+v", w)
	}
}

func TestNilRegistryIsInert(t *testing.T) {
	var r *Registry
	r.Record(dep, Attempt{})
	r.Retain(nil)
	if got := r.Project([]contract.DeploymentID{dep}); got != nil {
		t.Fatalf("nil registry projected %v", got)
	}
}

func TestToolCallErrorsAreCountedPerAttemptNotPerCall(t *testing.T) {
	r := NewRegistry(newClock().now)
	r.Record(dep, Attempt{Outcome: Succeeded, ToolCalls: 3})                          // clean
	r.Record(dep, Attempt{Outcome: Succeeded, ToolCalls: 4, MalformedToolCall: true}) // one bad attempt
	r.Record(dep, Attempt{Outcome: Succeeded})                                        // no tools: not in the denominator
	r.Record(dep, Attempt{Outcome: Failed, ToolCalls: 1, MalformedToolCall: true})    // failed attempts are not judged
	w := window(t, r, 5*time.Minute)
	if w.ToolCallAttempts != 2 || w.ToolCallErrors != 1 {
		t.Fatalf("tool call attempts/errors = %d/%d, want 2/1", w.ToolCallAttempts, w.ToolCallErrors)
	}
}
