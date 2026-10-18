package httpapi_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// The signed envelope lane serves no decisions: they are an ordinary grant on
// https://kaana.ai/v1/decisions. The retired route is gone and a decisions
// envelope on the inference route is refused before any adapter runs.
func TestTheSignedLaneServesNoDecisions(t *testing.T) {
	for _, path := range []string{"/internal/v1/decisions", "/internal/v1/inference"} {
		t.Run(path, func(t *testing.T) {
			adapter := &stubAdapter{}
			h := newHarness(t, adapter)
			body := envelope(t, func(body map[string]any) {
				body["stream"] = false
				body["input"] = map[string]any{"format": "decisions", "decisions": map[string]any{"state": "synthetic state", "questions": []any{map[string]any{"id": "question-1", "kind": "noul", "question": "Synthetic proposition?"}}}}
				body["client"].(map[string]any)["apiFormat"] = "decisions"
				body["client"].(map[string]any)["endpoint"] = "/v1/decisions"
			})
			request, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			h.sign(request, body)
			response, err := h.server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			raw, _ := io.ReadAll(response.Body)
			want := http.StatusBadRequest
			if path == "/internal/v1/decisions" {
				want = http.StatusNotFound
			}
			if response.StatusCode != want {
				t.Fatalf("status %d, want %d: %s", response.StatusCode, want, raw)
			}
			if _, _, calls := adapter.snapshot(); calls != 0 {
				t.Fatal("a signed decisions envelope reached an adapter")
			}
		})
	}
}
