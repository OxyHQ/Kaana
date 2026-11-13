package httpapi_test

import (
	"net/http"
	"testing"
)

// Metrics are CloudWatch EMF lines on stdout (internal/emf), which needs no
// scraper. A signed Prometheus route was added once and nothing ever read it;
// this pins that it stays removed, signed or not.
func TestThereIsNoMetricsRoute(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	for _, signed := range []bool{false, true} {
		request, err := http.NewRequest(http.MethodGet, harness.server.URL+"/internal/v1/metrics", nil)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		if signed {
			harness.sign(request, nil)
		}
		response, err := harness.server.Client().Do(request)
		if err != nil {
			t.Fatalf("requesting the route: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("signed=%v: /internal/v1/metrics was answered %d, want 404", signed, response.StatusCode)
		}
	}
}
