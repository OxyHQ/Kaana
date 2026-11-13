package publicapi_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/OxyHQ/Kaana/internal/emf"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

type recordedMetric struct {
	name   string
	value  float64
	labels emf.Labels
}

type recordingMetrics struct {
	mu       sync.Mutex
	recorded []recordedMetric
}

func (r *recordingMetrics) Record(metric emf.Metric, value float64, labels emf.Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, recordedMetric{metric.Name, value, labels})
}

func (r *recordingMetrics) named(name string) []recordedMetric {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedMetric
	for _, m := range r.recorded {
		if m.name == name {
			out = append(out, m)
		}
	}
	return out
}

// TestAServedRequestIsMeasuredWithClosedLabelsOnly: the metrics an operator
// alarms on are produced by the real handlers, and carry the operation and the
// outcome, never an id, a model or a provider (cardinality is the cost control).
func TestAServedRequestIsMeasuredWithClosedLabelsOnly(t *testing.T) {
	metrics := &recordingMetrics{}
	h := newHarness(t, harnessOptions{metrics: metrics})
	response := h.post("/v1/responses", validKey, responsesBody(nil))
	if body := readAll(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	h.settle()

	requests := metrics.named("PublicRequestLatency")
	if len(requests) != 1 {
		t.Fatalf("PublicRequestLatency recorded %d times, want 1", len(requests))
	}
	if requests[0].labels["Operation"] != "responses" || requests[0].labels["Outcome"] != "completed" || len(requests[0].labels) != 2 {
		t.Errorf("labels = %v, want exactly Operation and Outcome", requests[0].labels)
	}
	if len(metrics.named("GrantLatency")) != 1 {
		t.Error("the grant round trip was not measured")
	}
	if len(metrics.named("SettlementWritten")) != 1 || len(metrics.named("SettlementWriteFailed")) != 0 {
		t.Errorf("settlement outbox counters = written %d failed %d", len(metrics.named("SettlementWritten")), len(metrics.named("SettlementWriteFailed")))
	}
}

func TestARateLimitedRequestIsCounted(t *testing.T) {
	metrics := &recordingMetrics{}
	h := newHarness(t, harnessOptions{metrics: metrics, rate: publicapi.RateLimit{PerSecond: 0.001, Burst: 1}})
	h.post("/v1/responses", validKey, responsesBody(nil))
	if second := h.post("/v1/responses", validKey, responsesBody(nil)); second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", second.StatusCode)
	}
	if got := len(metrics.named("RateLimited")); got != 1 {
		t.Errorf("RateLimited recorded %d times, want 1", got)
	}
}
