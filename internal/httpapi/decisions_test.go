package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func decisionEnvelope(t *testing.T) []byte {
	t.Helper()
	return envelope(t, func(body map[string]any) {
		body["stream"] = false
		body["input"] = map[string]any{"format": "decisions", "decisions": map[string]any{"state": "synthetic private state", "questions": []any{map[string]any{"id": "question-1", "kind": "noul", "question": "Synthetic proposition?"}}}}
		body["client"].(map[string]any)["apiFormat"] = "decisions"
		body["client"].(map[string]any)["endpoint"] = "/v1/decisions"
	})
}
func TestSignedDecisionsEndpoint(t *testing.T) {
	for _, mode := range []string{"signed", "unsigned", "tampered", "wrong-family", "wrong-endpoint"} {
		t.Run(mode, func(t *testing.T) {
			adapter := &stubAdapter{}
			h := newHarness(t, adapter)
			body := decisionEnvelope(t)
			path := "/internal/v1/decisions"
			if mode == "wrong-family" {
				body = envelope(t, nil)
			}
			if mode == "wrong-endpoint" {
				path = "/internal/v1/inference"
			}
			r, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if mode != "unsigned" {
				h.sign(r, body)
			}
			if mode == "tampered" {
				r.Body = io.NopCloser(bytes.NewReader(bytes.Replace(body, []byte("question-1"), []byte("question-2"), 1)))
			}
			response, err := h.server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "signed" {
				if response.StatusCode == http.StatusOK {
					t.Fatalf("unsafe request accepted: %s", raw)
				}
				_, _, calls := adapter.snapshot()
				if calls != 0 {
					t.Fatal("rejected request reached adapter")
				}
				return
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", response.StatusCode, raw)
			}
			var result contract.DecisionResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if result.SchemaVersion != 1 || result.RequestID != "req_test" || result.Model != "stub/model@2026-05-01" || result.Usage.RequestID != result.RequestID || result.Usage.ResolvedModelReference != result.Model || result.Usage.Outcome != contract.OutcomeCompleted || len(result.Data) != 1 || result.Data[0].ID != "question-1" || result.Usage.ServingProvider != "stub" {
				t.Fatalf("bad result: %+v", result)
			}
			if strings.Contains(string(raw), "cost") || strings.Contains(string(raw), "synthetic private state") || strings.Contains(h.logs.String(), "synthetic private state") {
				t.Fatal("cost or content escaped")
			}
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Error("response can be cached")
			}
		})
	}
}
