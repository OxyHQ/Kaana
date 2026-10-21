package publicapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// timingLines returns every "public request timing" line the harness logged.
func timingLines(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if !strings.Contains(line, `"msg":"public request timing"`) {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("timing line is not JSON: %s", line)
		}
		lines = append(lines, decoded)
	}
	return lines
}

// A served request writes one timing line: its ids, the grant's round trip
// to Oxy measured as an exchange, the executor's time and the remainder that
// is Kaana's own. Nothing the caller sent is in it.
func TestAServedRequestLogsWhereItsTimeWent(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/responses", validKey, responsesBody(nil))
	if body := readAll(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	h.settle()
	lines := timingLines(t, h)
	if len(lines) != 1 {
		t.Fatalf("timing lines = %d, want 1:\n%s", len(lines), h.logs.String())
	}
	line := lines[0]
	if line["requestId"] != response.Header.Get("Kaana-Request-Id") || line["grantId"] != "grt_1" ||
		line["operation"] != "responses" || line["endpoint"] != "/v1/responses" || line["outcome"] != "completed" {
		t.Errorf("identity = %v", line)
	}
	for _, field := range []string{"totalMs", "beforeGrantMs", "grantMs", "executeMs", "kaanaMs", "oxyMs", "upstreamMs",
		"upstreamExchanges", "evidenceWrites", "evidenceWriteMs", "retryWaitMs"} {
		if _, ok := line[field].(float64); !ok {
			t.Errorf("%s = %v, want a number", field, line[field])
		}
	}
	// The grant crossed the wire to (fake) Oxy, so it is an exchange with a
	// status; the stub provider is not HTTP, so no upstream exchange is.
	exchanges, _ := line["exchanges"].([]any)
	if len(exchanges) != 1 {
		t.Fatalf("exchanges = %v, want the grant alone", line["exchanges"])
	}
	if grant, _ := exchanges[0].(map[string]any); grant["peer"] != "oxy" || grant["status"] != float64(http.StatusOK) {
		t.Errorf("grant exchange = %v", exchanges[0])
	}
	if line["upstreamExchanges"] != float64(0) {
		t.Errorf("upstreamExchanges = %v", line["upstreamExchanges"])
	}
	logs := h.logs.String()
	for _, leak := range []string{"secreto", validKey, kaanaToken} {
		if strings.Contains(logs, leak) {
			t.Errorf("the logs carry %q", leak)
		}
	}
	if !strings.Contains(logs, `"msg":"public request settlement"`) || !strings.Contains(logs, `"written":true`) {
		t.Errorf("no settlement timing line:\n%s", logs)
	}
}

// A request Oxy refuses still says how long the refusal took, with the
// refusal's code; a request refused before Oxy was asked writes no line.
func TestARefusedRequestLogsTheGrantRoundTripOnly(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	if response := h.post("/v1/responses", revokedKey, responsesBody(nil)); response.StatusCode == http.StatusOK {
		t.Fatalf("a revoked key was served")
	}
	if response := h.post("/v1/responses", validKey, []byte(`{"model":`)); response.StatusCode == http.StatusOK {
		t.Fatalf("an unreadable body was served")
	}
	lines := timingLines(t, h)
	if len(lines) != 1 {
		t.Fatalf("timing lines = %d, want 1 (the refused grant):\n%s", len(lines), h.logs.String())
	}
	if lines[0]["outcome"] == "completed" || lines[0]["outcome"] == "" || lines[0]["grantId"] != "" || lines[0]["executeMs"] != float64(0) {
		t.Errorf("refused line = %v", lines[0])
	}
}
