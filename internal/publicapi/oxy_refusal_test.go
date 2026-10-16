package publicapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// The body Oxy's grant API answers when a key is over its machine-lane limit
// (oxy packages/api/src/routes/internalKaana.ts, `machineLimit`): the public
// error, retryable, with the wait until the key's window resets.
const oxyMachineLaneRefusal = `{"schemaVersion":1,"code":"rate_limited","message":"This API key or application is over its request limit. Slow down and retry.","retryable":true,"requestId":"req_oxyratelimited000000000000","retryAfterMs":41250}`

func refuseWith(status int, contentType, body string, headers ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for index := 0; index+1 < len(headers); index += 2 {
			w.Header().Set(headers[index], headers[index+1])
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// A key over Oxy's limit is the caller's to slow down: 429 rate_limited with
// Oxy's wait, never a 503 that blames Kaana.
func TestOxysRateLimitedRefusalReachesTheCallerAsRateLimited(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.oxy.refusal = refuseWith(http.StatusTooManyRequests, "application/json; charset=utf-8", oxyMachineLaneRefusal, "Retry-After", "42")

	response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
	failure := publicError(t, response, http.StatusTooManyRequests)
	if failure.Code != "rate_limited" || !failure.Retryable || failure.RetryAfterMs == nil || *failure.RetryAfterMs != 41250 {
		t.Errorf("rate limited by Oxy = %+v", failure)
	}
	if got := response.Header.Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want 42", got)
	}
	if strings.Contains(string(response.Body), "API key or application") {
		t.Error("Oxy's own wording reached the caller")
	}
	if strings.Contains(h.logs.String(), "without a readable error") {
		t.Error("Oxy's public error was logged as unreadable")
	}
	if h.adapter.called() != 0 {
		t.Error("a refused request reached the provider")
	}
}

// Anything else Oxy answers 429 with is Oxy turning Kaana away, not a verdict
// on the caller: 503, retryable after the wait Oxy stated.
func TestAnUnreadableRefusalIsUnavailableForTheWaitOxyStated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter []string
		wantMs     int
		wantHeader string
	}{
		{"a stated wait", []string{"Retry-After", "120"}, 120_000, "120"},
		{"no stated wait", nil, 1000, "1"},
		{"a date, which Kaana does not read", []string{"Retry-After", "Fri, 16 Oct 2026 06:00:00 GMT"}, 1000, "1"},
		{"a wait past Oxy's longest window", []string{"Retry-After", "86400"}, 900_000, "900"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			// express-rate-limit's default answer: its message as text.
			h.oxy.refusal = refuseWith(http.StatusTooManyRequests, "text/html; charset=utf-8", "Too many requests, please try again later.", tc.retryAfter...)

			response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
			failure := publicError(t, response, http.StatusServiceUnavailable)
			if failure.Code != "service_unavailable" || !failure.Retryable || failure.RetryAfterMs == nil || *failure.RetryAfterMs != tc.wantMs {
				t.Errorf("unreadable refusal = %+v", failure)
			}
			if got := response.Header.Get("Retry-After"); got != tc.wantHeader {
				t.Errorf("Retry-After = %q, want %s", got, tc.wantHeader)
			}
			if !strings.Contains(h.logs.String(), "without a readable error") {
				t.Error("the unreadable refusal was not logged")
			}
		})
	}
}

// Oxy's global limiters answer its API error (oxy
// packages/api/src/middleware/rateLimiter.ts). Over Kaana's own service
// credential that is all of Kaana's control traffic, so it is still not a
// verdict on the caller — 503 for Oxy's wait — but Kaana reads it by name and
// logs it as Oxy rate-limiting Kaana, never as an unreadable answer.
func TestOxyRateLimitingKaanaItselfIsUnavailableForOxysWait(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.oxy.refusal = refuseWith(http.StatusTooManyRequests, "application/json; charset=utf-8",
		`{"error":"RATE_LIMITED","message":"Too many requests for this service credential, please slow down.","details":{"retryAfterSeconds":37}}`,
		"Retry-After", "37")

	response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
	failure := publicError(t, response, http.StatusServiceUnavailable)
	if failure.Code != "service_unavailable" || !failure.Retryable || failure.RetryAfterMs == nil || *failure.RetryAfterMs != 37_000 {
		t.Errorf("Oxy rate-limiting Kaana = %+v", failure)
	}
	if got := response.Header.Get("Retry-After"); got != "37" {
		t.Errorf("Retry-After = %q, want 37", got)
	}
	if strings.Contains(string(response.Body), "service credential") {
		t.Error("Oxy's own wording reached the caller")
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "Oxy is rate-limiting Kaana's own authorizations") || strings.Contains(logs, "without a readable error") {
		t.Errorf("logs = %s", logs)
	}
	if h.adapter.called() != 0 {
		t.Error("a refused request reached the provider")
	}
}

// Oxy's shared shape nested under `error` is read like the flat one.
func TestOxysNestedPublicErrorIsReadLikeTheFlatOne(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.oxy.refusal = refuseWith(http.StatusTooManyRequests, "application/json; charset=utf-8",
		`{"error":{"code":"rate_limited","retryAfterMs":5000}}`)

	response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
	failure := publicError(t, response, http.StatusTooManyRequests)
	if failure.Code != "rate_limited" || failure.RetryAfterMs == nil || *failure.RetryAfterMs != 5000 {
		t.Errorf("nested refusal = %+v", failure)
	}
}
