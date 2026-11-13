package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type metricsResult struct {
	StatusCode int
	Header     http.Header
}

func getMetrics(t *testing.T, harness *harness, signed bool) (metricsResult, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, harness.server.URL+"/internal/v1/metrics", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if signed {
		harness.sign(request, nil)
	}
	response, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting metrics: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading metrics: %v", err)
	}
	return metricsResult{StatusCode: response.StatusCode, Header: response.Header}, string(body)
}

func TestMetricsRequireASignature(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})

	unsigned, body := getMetrics(t, harness, false)
	if unsigned.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned metrics were answered %d", unsigned.StatusCode)
	}
	if strings.Contains(body, "# TYPE") || strings.Contains(body, "dep_stub") {
		t.Errorf("an unsigned refusal carried metric content: %s", body)
	}

	forged, err := http.NewRequest(http.MethodGet, harness.server.URL+"/internal/v1/metrics", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	forged.Header.Set("X-Oxy-Key-Id", harness.keyID)
	forged.Header.Set("X-Oxy-Signature", "v1=AAAA")
	response, err := harness.server.Client().Do(forged)
	if err != nil {
		t.Fatalf("requesting metrics: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("a forged signature was answered %d", response.StatusCode)
	}

	signedResponse, text := getMetrics(t, harness, true)
	if signedResponse.StatusCode != http.StatusOK {
		t.Fatalf("signed metrics were answered %d: %s", signedResponse.StatusCode, text)
	}
	if got := signedResponse.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := signedResponse.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestMetricsProjectTheInventoryAndNothingElse(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	_, text := getMetrics(t, harness, true)

	for _, want := range []string{
		`kaana_build_info{contract_version="` + contract.ContractVersion + `"} 1`,
		`kaana_inventory_deployments 1`,
		`kaana_inventory_serves_unpinned_references 1`,
		`kaana_deployment_breaker_state{deployment_id="dep_stub",provider="stub"} 0`,
		`kaana_provider_status{provider="stub",status="ok"} 1`,
		`kaana_deployment_window_attempts{deployment_id="dep_stub",provider="stub",window="5m"} 0`,
		`kaana_deployment_window_attempts{deployment_id="dep_stub",provider="stub",window="1d"} 0`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics lack %q:\n%s", want, text)
		}
	}
	// The inventory carries an upstream model id and a snapshot id; neither is
	// a metric label or value.
	for _, forbidden := range []string{"provider-private-route-name", "snap_stub", "test-region", "stub/model"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("metrics contain %q:\n%s", forbidden, text)
		}
	}
}

func TestMetricsOnlyAnswerGET(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	request, err := http.NewRequest(http.MethodPost, harness.server.URL+"/internal/v1/metrics", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	harness.sign(request, []byte("{}"))
	response, err := harness.server.Client().Do(request)
	if err != nil {
		t.Fatalf("requesting metrics: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST was answered %d", response.StatusCode)
	}
}
