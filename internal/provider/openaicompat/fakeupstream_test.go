package openaicompat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/provider/conformance"
)

// fakeUpstream speaks the REAL OpenAI Chat Completions wire format.
//
// That is the whole point of it. A fake that spoke the normalized contract
// would exercise none of the translation, and translation is where an adapter
// is most likely to be wrong — it is the half that has no schema to check it.
//
// It also observes its own cancellation. `r.Context()` is cancelled by net/http
// when the caller's connection goes away, so the count it records is an
// observation made at the server, about the connection, and not a claim the
// test makes about the client.

const chunkInterval = 40 * time.Millisecond

type fakeUpstream struct {
	scenario conformance.Scenario

	mutex                sync.Mutex
	written              int
	cancelledAfterChunks int
	requests             int
	seenAuthorization    string
	// firstCredential is the first credential this upstream was ever sent. It
	// is what ScenarioFirstCredentialExhausted refuses, so the scenario is
	// about WHICH key arrived rather than about how many requests have been
	// made — a counter would refuse the second key on a retry of the first.
	firstCredential string
}

func startFakeUpstream(t *testing.T, scenario conformance.Scenario) *conformance.Upstream {
	t.Helper()
	fake := &fakeUpstream{scenario: scenario, cancelledAfterChunks: -1}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	return &conformance.Upstream{
		URL:         server.URL,
		TotalChunks: totalChunksFor(scenario),
		Written: func() int {
			fake.mutex.Lock()
			defer fake.mutex.Unlock()
			return fake.written
		},
		CancelledAfterChunks: func() int {
			fake.mutex.Lock()
			defer fake.mutex.Unlock()
			return fake.cancelledAfterChunks
		},
		RequestCount: func() int {
			fake.mutex.Lock()
			defer fake.mutex.Unlock()
			return fake.requests
		},
	}
}

func totalChunksFor(scenario conformance.Scenario) int {
	switch scenario {
	case conformance.ScenarioStreaming, conformance.ScenarioNoUsage, conformance.ScenarioFirstCredentialExhausted:
		return 3
	case conformance.ScenarioSlowStream:
		return 6
	case conformance.ScenarioToolCalls:
		return 3
	case conformance.ScenarioMidStreamError:
		return 2
	default:
		return 0
	}
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		return
	}

	f.mutex.Lock()
	f.requests++
	f.seenAuthorization = r.Header.Get("Authorization")
	authorization := f.seenAuthorization
	if f.firstCredential == "" {
		f.firstCredential = authorization
	}
	firstCredential := f.firstCredential
	f.mutex.Unlock()

	if f.scenario == conformance.ScenarioFirstCredentialExhausted {
		if authorization == firstCredential {
			// The account behind THIS key has nothing left. On this protocol
			// that arrives as a 429 that a burst limit also uses, which is
			// exactly why the adapter classifies from the error type.
			writeUpstreamError(w, http.StatusTooManyRequests, "insufficient_quota", "you exceeded your current quota")
			return
		}
		f.writeStream(w, r)
		return
	}

	switch f.scenario {
	case conformance.ScenarioRateLimited:
		w.Header().Set("Retry-After", "2")
		writeUpstreamError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests, slow down")
		return
	case conformance.ScenarioQuotaExhausted:
		writeUpstreamError(w, http.StatusTooManyRequests, "insufficient_quota", "you exceeded your current quota")
		return
	case conformance.ScenarioRequestRefused:
		// The credential was fine; the request was not. Nothing another key
		// could change.
		writeUpstreamError(w, http.StatusBadRequest, "invalid_request_error", "the request body names an unknown parameter")
		return
	case conformance.ScenarioCredentialRefused:
		writeUpstreamError(w, http.StatusUnauthorized, "invalid_api_key", "incorrect api key provided")
		return
	case conformance.ScenarioCredentialEchoed:
		// Providers really do echo the request back in an error. This is the
		// single most likely way an upstream credential reaches a customer, so
		// the suite reproduces it rather than assuming it away.
		writeUpstreamError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("request rejected: headers were {Authorization: %s}", authorization))
		return
	case conformance.ScenarioNonStreaming:
		f.writeCompletion(w)
		return
	}

	f.writeStream(w, r)
}

func writeUpstreamError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": kind},
	})
}

func (f *fakeUpstream) writeCompletion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{
		"id":"chatcmpl-fake",
		"choices":[{"index":0,"message":{"role":"assistant","content":"a complete answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":5,
		         "prompt_tokens_details":{"cached_tokens":3},
		         "completion_tokens_details":{"reasoning_tokens":2}}
	}`))
}

func (f *fakeUpstream) writeStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "the test server cannot stream", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	total := totalChunksFor(f.scenario)
	for index := range total {
		frame := f.chunkFor(index)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
			return
		}
		flusher.Flush()

		f.mutex.Lock()
		f.written++
		f.mutex.Unlock()

		select {
		case <-r.Context().Done():
			// Recorded at the server, about this connection. The conformance
			// suite compares it against a control run that was never
			// cancelled, which is what distinguishes "the disconnect
			// propagated" from "the request simply ended".
			f.mutex.Lock()
			f.cancelledAfterChunks = f.written
			f.mutex.Unlock()
			return
		case <-time.After(chunkInterval):
		}
	}

	if f.scenario == conformance.ScenarioMidStreamError {
		// A 200 was already sent and output already streamed, so this failure
		// has no HTTP status and never will: this protocol spells it as an
		// error object where a chunk belongs.
		_, _ = fmt.Fprint(w, `data: {"error":{"message":"the engine is currently overloaded","type":"server_error","code":null}}`+"\n\n")
		flusher.Flush()
		return
	}

	// The terminal frame carries the finish reason, and the usage frame after
	// it is what `stream_options.include_usage` buys.
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\""+f.finishReason()+"\"}]}\n\n")
	flusher.Flush()

	if f.scenario != conformance.ScenarioNoUsage {
		_, _ = fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`+"\n\n")
		flusher.Flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (f *fakeUpstream) finishReason() string {
	if f.scenario == conformance.ScenarioToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// chunkFor produces one frame of the scenario's output, in the provider's own
// shape — including the protocol's habit of sending a tool call's id and name
// once and then only argument fragments.
func (f *fakeUpstream) chunkFor(index int) string {
	if f.scenario == conformance.ScenarioToolCalls {
		switch index {
		case 0:
			return `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_fake","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`
		case 1:
			return `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`
		default:
			return `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"relay\"}"}}]}}]}`
		}
	}
	if index == 0 {
		return `{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"}}]}`
	}
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":"chunk %d "}}]}`, index)
}
