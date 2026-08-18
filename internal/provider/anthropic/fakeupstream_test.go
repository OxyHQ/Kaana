package anthropic

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

// fakeUpstream speaks the REAL Anthropic Messages API wire format.
//
// That is the whole point of it. A fake that spoke the normalized contract
// would exercise none of the translation, and translation is where an adapter
// is most likely to be wrong — it is the half that has no schema to check it.
// Every frame below is the shape the published documentation shows: named SSE
// events, indexed content blocks, a cumulative output count on the final
// `message_delta`, and no terminal sentinel.
//
// It also observes its own cancellation. `r.Context()` is cancelled by net/http
// when the caller's connection goes away, so the count it records is an
// observation made at the server, about the connection, and not a claim the
// test makes about the client.

const chunkInterval = 40 * time.Millisecond

// The token counts every scenario reports, and the physical request they
// describe. They are stated here once, in the provider's own fields, and
// restated in the conformance subject as the totals the contract's units have
// to partition — so the two can disagree and be caught, rather than being one
// number read twice.
//
//	input_tokens              7   uncached prompt tokens, EXCLUDING both cache counts
//	cache_creation_input_tokens 2 prompt tokens written to the cache
//	cache_read_input_tokens   3   prompt tokens served from the cache
//	output_tokens             5   every generated token, thinking INCLUDED
//	thinking_tokens           2   how many of those were thinking
//
// So the request physically consumed 12 prompt tokens (7+2+3) of which 3 were
// cached, and produced 5 output tokens of which 2 were reasoning.
const (
	fakeInputTokens         = 7
	fakeCacheCreationTokens = 2
	fakeCacheReadTokens     = 3
	fakeOutputTokens        = 5
	fakeThinkingTokens      = 2
	// fakeOpeningOutputTokens is what `message_start` reports before anything
	// has been generated. It is deliberately non-zero and deliberately not the
	// final count: an adapter that ADDS the two numbers this protocol sends
	// reports 6 output tokens for a request that produced 5.
	fakeOpeningOutputTokens = 1
)

type fakeUpstream struct {
	scenario conformance.Scenario

	mutex                sync.Mutex
	written              int
	cancelledAfterChunks int
	requests             int
	seenAPIKey           string
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
		if r.Header.Get("anthropic-version") == "" {
			// The provider requires the version header on every request,
			// including this one. Answering without it would let a health probe
			// pass that a real provider would reject.
			writeUpstreamError(w, http.StatusBadRequest, errorInvalidRequest, "anthropic-version is required")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"first_id":null,"last_id":null}`))
		return
	}

	f.mutex.Lock()
	f.requests++
	f.seenAPIKey = r.Header.Get("x-api-key")
	apiKey := f.seenAPIKey
	f.mutex.Unlock()

	if r.Header.Get("anthropic-version") == "" {
		writeUpstreamError(w, http.StatusBadRequest, errorInvalidRequest, "anthropic-version is required")
		return
	}

	if f.scenario == conformance.ScenarioFirstCredentialExhausted {
		f.mutex.Lock()
		if f.firstCredential == "" {
			f.firstCredential = apiKey
		}
		exhausted := apiKey == f.firstCredential
		f.mutex.Unlock()
		if exhausted {
			// The account behind THIS key has nothing left. This provider says
			// so on a status no rate limit uses, where an OpenAI-compatible one
			// says it on a 429 — which is why the adapter classifies from the
			// error type and the invariant is the same for both.
			writeUpstreamError(w, http.StatusPaymentRequired, errorBilling, "your credit balance is too low to access the API")
			return
		}
		f.writeStream(w, r)
		return
	}

	switch f.scenario {
	case conformance.ScenarioRateLimited:
		w.Header().Set("retry-after", "2")
		writeUpstreamError(w, http.StatusTooManyRequests, errorRateLimit, "number of requests has exceeded your rate limit")
		return
	case conformance.ScenarioQuotaExhausted:
		// Not a 429. This provider carries an exhausted account on its own
		// status, which is exactly why an adapter that classified by status
		// alone would call this a rate limit and tell the client to retry.
		writeUpstreamError(w, http.StatusPaymentRequired, errorBilling, "your credit balance is too low to access the API")
		return
	case conformance.ScenarioCredentialRefused:
		writeUpstreamError(w, http.StatusUnauthorized, errorAuthentication, "invalid x-api-key")
		return
	case conformance.ScenarioRequestRefused:
		// The credential was fine; the request was not. Nothing another key
		// could change.
		writeUpstreamError(w, http.StatusBadRequest, errorInvalidRequest, "the request body names an unknown parameter")
		return
	case conformance.ScenarioCredentialEchoed:
		// Providers really do echo the request back in an error. This is the
		// single most likely way an upstream credential reaches a customer, so
		// the suite reproduces it rather than assuming it away — and it is
		// echoed under THIS provider's header name, which the contract's
		// credential-shaped-text pattern does not cover.
		writeUpstreamError(w, http.StatusBadRequest, errorInvalidRequest,
			fmt.Sprintf("request rejected: headers were {x-api-key: %s}", apiKey))
		return
	case conformance.ScenarioNonStreaming:
		f.writeMessage(w)
		return
	}

	f.writeStream(w, r)
}

func writeUpstreamError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": kind, "message": message},
		"request_id": "req_fake",
	})
}

// writeMessage is a complete, non-streamed response: one thinking block and one
// text block, with the whole usage object.
func (f *fakeUpstream) writeMessage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{
		"id":"msg_fake",
		"type":"message",
		"role":"assistant",
		"model":"claude-fake",
		"content":[
			{"type":"thinking","thinking":"working it out","signature":"c2lnbmF0dXJl"},
			{"type":"text","text":"a complete answer"}
		],
		"stop_reason":"end_turn",
		"stop_sequence":null,
		"usage":%s
	}`, f.usageJSON(fakeOutputTokens))
}

// usageJSON renders the provider's usage object. The output count is a
// parameter because this protocol reports it twice, cumulatively.
func (f *fakeUpstream) usageJSON(outputTokens int) string {
	return fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"output_tokens_details":{"thinking_tokens":%d}}`,
		fakeInputTokens, fakeCacheCreationTokens, fakeCacheReadTokens, outputTokens, fakeThinkingTokens)
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

	write := func(name, data string) bool {
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		flusher.Flush()
		return err == nil
	}

	// message_start carries the input accounting in the FIRST event, and a
	// provisional output count that the final message_delta supersedes.
	start := fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","model":"claude-fake","content":[],"stop_reason":null,"stop_sequence":null,"usage":%s}}`,
		f.usageJSON(fakeOpeningOutputTokens))
	if f.scenario == conformance.ScenarioNoUsage {
		// Some gateways in front of this protocol strip usage entirely, and the
		// published streaming examples include a message_start with no usage
		// object at all.
		start = `{"type":"message_start","message":{"id":"msg_fake","type":"message","role":"assistant","model":"claude-fake","content":[],"stop_reason":null,"stop_sequence":null}}`
	}
	if !write("message_start", start) {
		return
	}
	// A ping, which this protocol may send at any point and which carries
	// nothing. An adapter that treated an unknown event as output would emit it.
	if !write("ping", `{"type":"ping"}`) {
		return
	}

	if !f.writeBlocks(w, r, write) {
		return
	}

	if f.scenario == conformance.ScenarioMidStreamError {
		// A 200 was already sent and output already streamed, so this failure
		// has no HTTP status and never will.
		write("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		return
	}

	delta := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":%s}`,
		f.stopReason(), f.usageJSON(fakeOutputTokens))
	if f.scenario == conformance.ScenarioNoUsage {
		delta = fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null}}`, f.stopReason())
	}
	if !write("message_delta", delta) {
		return
	}
	write("message_stop", `{"type":"message_stop"}`)
}

// writeBlocks streams the scenario's content blocks. It returns false once the
// connection is gone.
func (f *fakeUpstream) writeBlocks(w http.ResponseWriter, r *http.Request, write func(name, data string) bool) bool {
	if f.scenario == conformance.ScenarioToolCalls {
		// A tool call declares its id and name in the block that OPENS it, and
		// then streams argument fragments that carry only an index.
		if !write("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_fake","name":"lookup","input":{}}}`) {
			return false
		}
		fragments := []string{`""`, `"{\"q\":"`, `"\"relay\"}"`}
		for _, fragment := range fragments {
			if !f.writeChunk(r, write, "content_block_delta",
				fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, fragment)) {
				return false
			}
		}
		return write("content_block_stop", `{"type":"content_block_stop","index":0}`)
	}

	// A thinking block first, closed by its signature, then the answer: the
	// order a real response arrives in, and the reason an adapter has to know
	// which block an index belongs to before it can decide which channel a
	// delta is on.
	if !write("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`) {
		return false
	}
	if !f.writeChunk(r, write, "content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"working it out"}}`) {
		return false
	}
	if !write("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pk"}}`) {
		return false
	}
	if !write("content_block_stop", `{"type":"content_block_stop","index":0}`) {
		return false
	}
	if !write("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`) {
		return false
	}
	for index := 1; index < totalChunksFor(f.scenario); index++ {
		if !f.writeChunk(r, write, "content_block_delta",
			fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"chunk %d "}}`, index)) {
			return false
		}
	}
	return write("content_block_stop", `{"type":"content_block_stop","index":1}`)
}

// writeChunk writes one counted output chunk and then yields, so a caller can
// disconnect mid-stream and the server can observe it.
func (f *fakeUpstream) writeChunk(r *http.Request, write func(name, data string) bool, name, data string) bool {
	if !write(name, data) {
		return false
	}
	f.mutex.Lock()
	f.written++
	f.mutex.Unlock()

	select {
	case <-r.Context().Done():
		// Recorded at the server, about this connection. The conformance suite
		// compares it against a control run that was never cancelled, which is
		// what distinguishes "the disconnect propagated" from "the request
		// simply ended".
		f.mutex.Lock()
		f.cancelledAfterChunks = f.written
		f.mutex.Unlock()
		return false
	case <-time.After(chunkInterval):
	}
	return true
}

func (f *fakeUpstream) stopReason() string {
	if f.scenario == conformance.ScenarioToolCalls {
		return "tool_use"
	}
	return "end_turn"
}
