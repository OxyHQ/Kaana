package publicapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/publicapi"
	"github.com/jackc/pgx/v5/pgconn"
)

// readOnlyRefusal is what PostgreSQL answers any write with while the database
// is frozen with default_transaction_read_only.
func readOnlyRefusal() *pgconn.PgError {
	return &pgconn.PgError{Severity: "ERROR", Code: "25006", Message: "cannot execute SELECT FOR UPDATE in a read-only transaction"}
}

func TestNativeAdmissionOnAReadOnlyDatabaseAnswers503WithRetryAfter(t *testing.T) {
	for name, err := range map[string]error{
		// nativeadmission returns the driver's error as it met it.
		"bare": readOnlyRefusal(),
		// commercial admission goes through billing's wrap.
		"wrapped": fmt.Errorf("commercialadmission: admit: %w", fmt.Errorf("billing: database operation: %w", readOnlyRefusal())),
	} {
		t.Run(name, func(t *testing.T) {
			config, store, catalogue := nativeFixture(t)
			store.admitErr = err
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			response := h.post("/v1/responses", validKey, map[string]any{"model": "auto", "input": "hello"})
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
			}
			if got := response.Header.Get("Retry-After"); got != "5" {
				t.Fatalf("Retry-After=%q", got)
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body, &body); err != nil {
				t.Fatal(err)
			}
			requestID := response.Header.Get(publicapi.HeaderRequestID)
			want := map[string]any{
				"schemaVersion": float64(1), "code": "service_unavailable",
				"message":   "the service is temporarily unavailable; retry later",
				"retryable": true, "requestId": requestID, "retryAfterMs": float64(5000),
			}
			if requestID == "" || len(body) != len(want) {
				t.Fatalf("body=%s requestId=%q", response.Body, requestID)
			}
			for key, value := range want {
				if body[key] != value {
					t.Fatalf("%s=%v, want %v; body=%s", key, body[key], value, response.Body)
				}
			}
			if h.adapter.called() != 0 {
				t.Fatal("a request the database refused to admit was executed")
			}
			h.oxy.mu.Lock()
			defer h.oxy.mu.Unlock()
			if h.oxy.grants != 0 {
				t.Fatal("a read-only refusal fell back to legacy authority")
			}
		})
	}
}

func TestReadOnlyRefusalKeepsEachDialectsErrorShape(t *testing.T) {
	for _, dialect := range []struct {
		path string
		body map[string]any
		want string
	}{
		{"/v1/chat/completions", map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, `"type":"api_error"`},
		{"/v1/messages", map[string]any{"model": "auto", "max_tokens": 8192, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, `"type":"error"`},
	} {
		t.Run(dialect.path, func(t *testing.T) {
			config, store, catalogue := nativeFixture(t)
			store.admitErr = fmt.Errorf("billing: database operation: %w", readOnlyRefusal())
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			response := h.post(dialect.path, validKey, dialect.body)
			if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "5" ||
				!strings.Contains(string(response.Body), dialect.want) {
				t.Fatalf("status=%d Retry-After=%q body=%s", response.StatusCode, response.Header.Get("Retry-After"), response.Body)
			}
		})
	}
}

func TestOtherDatabaseFailuresKeepTheirAnswer(t *testing.T) {
	config, store, catalogue := nativeFixture(t)
	// An administrator shutdown is not a freeze: it stays the unhinted
	// service_unavailable it was, with no promise that five seconds is enough.
	store.admitErr = fmt.Errorf("billing: database operation: %w", &pgconn.PgError{Code: "57P01"})
	h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
	response := h.post("/v1/responses", validKey, map[string]any{"model": "auto", "input": "hello"})
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "" ||
		strings.Contains(string(response.Body), "retryAfterMs") {
		t.Fatalf("status=%d Retry-After=%q body=%s", response.StatusCode, response.Header.Get("Retry-After"), response.Body)
	}
}

func TestReadOnlyCompletionRecordAnswers503WithRetryAfter(t *testing.T) {
	config, store, catalogue := nativeFixture(t)
	store.stageErr = fmt.Errorf("nativeadmission: stage completion: %w", readOnlyRefusal())
	h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
	response := h.post("/v1/responses", validKey, map[string]any{"model": "auto", "input": "hello"})
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "5" ||
		!strings.Contains(string(response.Body), `"retryAfterMs":5000`) {
		t.Fatalf("status=%d Retry-After=%q body=%s", response.StatusCode, response.Header.Get("Retry-After"), response.Body)
	}
}
