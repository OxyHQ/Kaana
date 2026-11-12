package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "oxy_sk_secret_key_value"

func streamServer(t *testing.T, firstDelay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "bad request echoing "+r.Header.Get("Authorization"), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		flusher.Flush()
		time.Sleep(firstDelay)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":40}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func TestMeasureTimesTheFirstOutputNotTheFirstFrame(t *testing.T) {
	server := streamServer(t, 60*time.Millisecond)
	defer server.Close()
	cfg := config{baseURL: server.URL + "/v1", key: testKey, prompt: "p", maxTokens: 8, runs: 1, client: server.Client()}
	got := measure(context.Background(), cfg, "stub/model")
	if got.Err != "" || got.OutputTokens != 40 {
		t.Fatalf("sample = %+v", got)
	}
	// The role-only frame arrives at once; the first content waits 60 ms.
	if got.TTFT < 50*time.Millisecond || got.Total < got.TTFT {
		t.Errorf("TTFT %v, total %v: the role frame was counted as output", got.TTFT, got.Total)
	}
}

// A refusal is a failure, never a fast answer, and the credential the server
// echoed never reaches the report.
func TestAFailedRunIsCountedAndRedacted(t *testing.T) {
	server := streamServer(t, 0)
	defer server.Close()
	cfg := config{baseURL: server.URL + "/v1", key: "wrong-key", prompt: "p", maxTokens: 8, runs: 2, client: server.Client()}
	var out bytes.Buffer
	if err := run(context.Background(), cfg, []string{"stub/model"}, &out); err != nil {
		t.Fatal(err)
	}
	var report summary
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Runs != 2 || report.Failures != 2 || report.TTFTp50Ms != 0 || !strings.Contains(report.FirstError, "HTTP 401") {
		t.Errorf("report = %+v", report)
	}
	if strings.Contains(out.String(), "wrong-key") {
		t.Errorf("the credential reached the report: %s", out.String())
	}
}

func TestSummarizeKeepsFailuresOutOfThePercentiles(t *testing.T) {
	samples := []sample{
		{TTFT: 100 * time.Millisecond, Total: 1100 * time.Millisecond, OutputTokens: 50},
		{TTFT: 300 * time.Millisecond, Total: 2300 * time.Millisecond, OutputTokens: 50},
		{Err: "HTTP 500"},
	}
	got := summarize("m", samples)
	if got.Runs != 3 || got.Failures != 1 || got.FirstError != "HTTP 500" {
		t.Fatalf("summary = %+v", got)
	}
	if got.TTFTp50Ms != 300 || got.TTFTp95Ms != 300 || got.OutputTokens != 50 {
		t.Errorf("percentiles = %+v", got)
	}
	// 50 tokens over 1.0 s and over 2.0 s: 50 and 25 tokens/s; the median of two
	// is the upper one by the rounding rule.
	if got.TokensPerSec != 50 {
		t.Errorf("tokens/s = %v", got.TokensPerSec)
	}
}
