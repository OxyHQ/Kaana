package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// LEGACY: remove with the shim once Oxy stops negotiating (OxyHQ/oxy#1809).
// The Oxy API in production reads the catalogue and the published deployments
// through the old scoped / Private Auto negotiation and refuses any answer
// that does not echo it exactly.
func TestLegacyNegotiatedReadsEchoTheExactVersionAndAddNoRows(t *testing.T) {
	harness := newHarness(t, &stubAdapter{})
	post := func(path, body string) (int, map[string]json.RawMessage, http.Header) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, harness.server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("building %s: %v", path, err)
		}
		request.Header.Set("Content-Type", "application/json")
		harness.sign(request, []byte(body))
		response, err := harness.server.Client().Do(request)
		if err != nil {
			t.Fatalf("requesting %s: %v", path, err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, _ := io.ReadAll(response.Body)
		var decoded map[string]json.RawMessage
		_ = json.Unmarshal(raw, &decoded)
		return response.StatusCode, decoded, response.Header
	}

	for _, negotiation := range []struct{ key, version string }{
		{"scopedExecutionContractVersion", "3.6.0"},
		{"privateAutoExecutionContractVersion", "3.7.0"},
	} {
		body := `{"` + negotiation.key + `":"` + negotiation.version + `"}`
		for _, path := range []string{"/internal/v1/models/query", "/internal/v1/deployments/query"} {
			status, decoded, header := post(path, body)
			if status != http.StatusOK || !strings.Contains(header.Get("Cache-Control"), "no-store") {
				t.Fatalf("%s %s was answered %d (Cache-Control %q)", path, body, status, header.Get("Cache-Control"))
			}
			if string(decoded[negotiation.key]) != `"`+negotiation.version+`"` {
				t.Errorf("%s %s echoed %s", path, body, decoded[negotiation.key])
			}
			if string(decoded["deployments"]) == "" || !strings.Contains(string(decoded["deployments"]), `"dep_stub"`) ||
				strings.Contains(string(decoded["deployments"]), "scopedExecution") {
				t.Errorf("%s %s deployments are not the ordinary projection: %s", path, body, decoded["deployments"])
			}
			if path == "/internal/v1/models/query" && !strings.Contains(string(decoded["models"]), `"stub/model"`) {
				t.Errorf("the negotiated catalogue is not the ordinary catalogue: %s", decoded["models"])
			}
		}
	}

	for _, refused := range []struct{ path, body string }{
		{"/internal/v1/models/query", `{}`},
		{"/internal/v1/models/query", `{"scopedExecutionContractVersion":"3.5.0"}`},
		{"/internal/v1/models/query", `{"scopedExecutionContractVersion":"3.6.0","deploymentIds":["dep_stub"]}`},
		{"/internal/v1/deployments/query", `{"scopedExecutionContractVersion":"3.6.0","privateAutoExecutionContractVersion":"3.7.0"}`},
		{"/internal/v1/deployments/query", `{"privateAutoExecutionContractVersion":"3.6.0"}`},
	} {
		if status, _, _ := post(refused.path, refused.body); status != http.StatusBadRequest {
			t.Errorf("%s %s was answered %d, want 400", refused.path, refused.body, status)
		}
	}
}
