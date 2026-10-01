package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
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

// postDecisions returns the status, headers and body of one signed request.
func postDecisions(t *testing.T, h *harness) (int, http.Header, []byte) {
	t.Helper()
	body := decisionEnvelope(t)
	r, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/v1/decisions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	h.sign(r, body)
	response, err := h.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, raw
}

// A refusal before any provider attempt is a 4xx Oxy reads as "not executed".
func TestDecisionsPreSpendRefusalsAre4xx(t *testing.T) {
	for _, tc := range []struct {
		code   contract.ErrorCode
		status int
	}{
		{contract.CodePermissionDenied, http.StatusForbidden},
		{contract.CodeInvalidRequest, http.StatusBadRequest},
		{contract.CodeRequestTooLarge, http.StatusRequestEntityTooLarge},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			h := newHarness(t, &stubAdapter{refuse: provider.ErrUnsupported{Code: tc.code, Detail: "synthetic refusal"}})
			status, _, raw := postDecisions(t, h)
			if status != tc.status {
				t.Fatalf("status %d: %s", status, raw)
			}
			var failure contract.Error
			if err := json.Unmarshal(raw, &failure); err != nil || failure.Code != tc.code || failure.RequestID != "req_test" {
				t.Fatalf("body %s", raw)
			}
		})
	}
}

// After dispatch, a failure is a typed DecisionFailure: never retryable, with
// usage exactly when the provider measured something.
func TestDecisionsFailureAfterDispatch(t *testing.T) {
	for _, measured := range []bool{true, false} {
		t.Run(map[bool]string{true: "measured", false: "unmeasured"}[measured], func(t *testing.T) {
			adapter := &stubAdapter{fail: provider.ErrUpstream{Code: contract.CodeProviderTimeout, Category: contract.UpstreamTimeout, Detail: "synthetic deadline", RetryAfterMs: 1000}}
			if measured {
				adapter.failUnits = []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 7}, {Unit: contract.UnitRequests, Quantity: 1}}
			}
			h := newHarness(t, adapter)
			status, header, raw := postDecisions(t, h)
			if status != http.StatusBadGateway || header.Get("Cache-Control") != "no-store" {
				t.Fatalf("status %d: %s", status, raw)
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			var failure contract.DecisionFailure
			if err := decoder.Decode(&failure); err != nil {
				t.Fatalf("not a typed DecisionFailure: %v %s", err, raw)
			}
			if err := failure.Validate(); err != nil {
				t.Fatal(err)
			}
			if failure.Error.Code != contract.CodeProviderTimeout || failure.Error.Retryable || failure.Error.RetryAfterMs != nil {
				t.Fatalf("failure invites a retry: %+v", failure.Error)
			}
			if measured != (failure.Usage != nil) {
				t.Fatalf("usage present=%v, measured=%v: %s", failure.Usage != nil, measured, raw)
			}
			if measured && (failure.Usage.Units[0].Quantity != 7 || failure.Usage.Outcome != contract.OutcomePartial) {
				t.Fatalf("measured usage changed: %+v", failure.Usage)
			}
			if !measured && strings.Contains(string(raw), `"usage"`) {
				t.Fatal("unmeasured failure carries usage")
			}
		})
	}
}

func TestSignedDecisionsUnicodeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, replacement    string
		root, options, valid bool
	}{
		{name: "state-high", replacement: "\\ud800"},
		{name: "state-low", replacement: "\\udc00"},
		{name: "root-key", replacement: "\\ud800", root: true},
		{name: "options", replacement: "\\ud800", options: true},
		{name: "invalid-utf8", replacement: string([]byte{0xff})},
		{name: "pair", replacement: "\\ud83d\\ude00", valid: true},
		{name: "replacement", replacement: "�", valid: true},
		{name: "escaped-replacement", replacement: "\\ufffd", valid: true},
		{name: "unicode", replacement: "😀 español 中文", valid: true},
		{name: "literal-pattern", replacement: "\\\\ud800", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &stubAdapter{}
			h := newHarness(t, adapter)
			body := decisionEnvelope(t)
			if tc.root {
				body = append([]byte("{\""+tc.replacement+"\":0,"), body[1:]...)
			} else if tc.options {
				body = bytes.Replace(body, []byte("\"kind\":\"noul\""), []byte("\"kind\":\"choice\",\"options\":[\"safe\",\""+tc.replacement+"\"]"), 1)
			} else {
				body = bytes.Replace(body, []byte("synthetic private state"), []byte(tc.replacement), 1)
			}
			request, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/v1/decisions", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			h.sign(request, body)
			response, err := h.server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			_, _, calls := adapter.snapshot()
			if tc.valid {
				if response.StatusCode != http.StatusOK || calls != 1 {
					t.Fatalf("valid Unicode rejected: status=%d calls=%d body=%s", response.StatusCode, calls, raw)
				}
			} else {
				if response.StatusCode != http.StatusBadRequest || calls != 0 {
					t.Fatalf("invalid signed Unicode executed: status=%d calls=%d body=%s", response.StatusCode, calls, raw)
				}
				var failure contract.Error
				if err := json.Unmarshal(raw, &failure); err != nil || failure.Code != contract.CodeInvalidRequest {
					t.Fatalf("untyped refusal: %s", raw)
				}
			}
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("decisions refusal can be cached")
			}
		})
	}
}
