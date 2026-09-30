package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// sendThroughRealWire translates a request for one slug and streams it to a
// fake speaking the real Chat Completions wire, returning the exact bytes the
// upstream received. Inspecting the Call alone would prove serialization, not
// that these are the bytes the HTTP adapter sends.
func sendThroughRealWire(t *testing.T, slug contract.ProviderSlug, request *contract.Request) map[string]json.RawMessage {
	t.Helper()
	return sendThroughRealWireFor(t, slug, realWireModel(slug), request)
}

func sendThroughRealWireFor(t *testing.T, slug contract.ProviderSlug, upstreamModelID string, request *contract.Request) map[string]json.RawMessage {
	t.Helper()
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-reasoning","choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning_content":"thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":2}}}`))
	}))
	t.Cleanup(upstream.Close)

	baseURL, client := upstream.URL, (*http.Client)(nil)
	if reviewed, bound := identityBoundTestBaseURL(slug); bound {
		baseURL, client = reviewed, identityBoundFakeClient(t, upstream.URL)
	}
	adapter, err := New(Config{Provider: slug, BaseURL: baseURL, Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: client})
	if err != nil {
		t.Fatalf("building the %s adapter: %v", slug, err)
	}
	call, err := adapter.Translate(request, provider.Route{
		DeploymentID: "dep_reasoning", Provider: slug, ModelReference: "openai/gpt-oss-120b@observed-2026-08-06", UpstreamModelID: upstreamModelID,
	})
	if err != nil {
		t.Fatalf("translating for %s: %v", slug, err)
	}
	if _, err := adapter.Stream(context.Background(), call, silentEmitter{}, nil); err != nil {
		t.Fatalf("sending through %s: %v", slug, err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(<-received, &wire); err != nil {
		t.Fatalf("the upstream body is not JSON: %v", err)
	}
	return wire
}

// realWireModel is a model each slug really serves that takes every effort:
// xAI's efforts are stated per model, and gpt-oss is not an xAI model.
func realWireModel(slug contract.ProviderSlug) string {
	if slug == "xai" {
		return "grok-4.7"
	}
	return "openai/gpt-oss-120b"
}

func reasoningRequest(effort contract.ReasoningEffort) *contract.Request {
	request := requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("think")}}})
	request.Stream = false
	if effort != "" {
		request.Reasoning = &contract.ReasoningParameters{Effort: effort}
	}
	return request
}

func TestReasoningEffortReachesEachReviewedProviderInItsOwnField(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"openai", "groq", "cerebras", "xai"} {
		for _, effort := range contract.ReasoningEfforts() {
			wire := sendThroughRealWire(t, slug, reasoningRequest(effort))
			if string(wire["reasoning_effort"]) != `"`+string(effort)+`"` {
				t.Errorf("%s/%s: reasoning_effort on the wire = %s", slug, effort, wire["reasoning_effort"])
			}
			if _, leaked := wire["reasoning"]; leaked {
				t.Errorf("%s/%s: OpenRouter's reasoning object reached a direct provider", slug, effort)
			}
		}
	}
}

func TestReasoningEffortReachesOpenRouterAsItsReasoningObject(t *testing.T) {
	for _, effort := range contract.ReasoningEfforts() {
		wire := sendThroughRealWire(t, "openrouter", reasoningRequest(effort))
		if string(wire["reasoning"]) != `{"effort":"`+string(effort)+`"}` {
			t.Errorf("%s: reasoning on the OpenRouter wire = %s", effort, wire["reasoning"])
		}
		if _, leaked := wire["reasoning_effort"]; leaked {
			t.Errorf("%s: the direct-provider field reached OpenRouter", effort)
		}
		// The effort must not displace the non-negotiable routing policy:
		// require_parameters is what keeps OpenRouter from routing a reasoning
		// request to an upstream that ignores `reasoning`.
		assertExactOpenRouterProviderPolicy(t, wire["provider"])
	}
}

func TestAnAbsentEffortSendsNoReasoningFieldAnywhere(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"openrouter", "groq", "together"} {
		wire := sendThroughRealWire(t, slug, reasoningRequest(""))
		for _, field := range []string{"reasoning", "reasoning_effort"} {
			if _, present := wire[field]; present {
				t.Errorf("%s: %s was invented for a request that named no effort", slug, field)
			}
		}
	}
}

func TestAnUnreviewedProviderRefusesTheEffortBeforeSpendingAnything(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(upstream.Close)
	// The reviewed set is written out here rather than read back from
	// reasoningDialectFor, so adding a slug there without a wire test fails.
	reviewed := map[contract.ProviderSlug]bool{"openai": true, "groq": true, "cerebras": true, "xai": true, "openrouter": true}
	for slug, endpoint := range providerconfig.Known {
		if endpoint.Protocol != providerconfig.ProtocolOpenAICompatible || reviewed[slug] {
			continue
		}
		baseURL, client := upstream.URL, (*http.Client)(nil)
		if reviewed, bound := identityBoundTestBaseURL(slug); bound {
			baseURL, client = reviewed, identityBoundFakeClient(t, upstream.URL)
		}
		adapter, err := New(Config{Provider: slug, BaseURL: baseURL, Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: client})
		if err != nil {
			t.Fatalf("building %s: %v", slug, err)
		}
		_, err = adapter.Translate(reasoningRequest(contract.ReasoningEffortHigh), testRoute())
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != "reasoning.effort" || unsupported.Code != contract.CodeInvalidRequest {
			t.Errorf("%s: an effort with no reviewed wire field was not refused by name: %v", slug, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("a refused effort reached an upstream %d times", calls.Load())
	}
	// Control: the refusal is about the effort, not the provider.
	if _, err := testAdapter(t).Translate(reasoningRequest(""), testRoute()); err != nil {
		t.Errorf("control: a request without an effort was refused: %v", err)
	}
}

func TestAnEffortOnSpeechOrEmbeddingsIsRefusedNotDropped(t *testing.T) {
	speech := reasoningRequest(contract.ReasoningEffortLow)
	speech.Client.APIFormat = contract.APIFormatAudioSpeech
	embedding := reasoningRequest(contract.ReasoningEffortLow)
	embedding.Modality = contract.ModalityEmbedding
	for name, request := range map[string]*contract.Request{"speech": speech, "embedding": embedding} {
		_, err := testAdapter(t).Translate(request, testRoute())
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != "reasoning.effort" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// xAI's direct API answers an effort its model does not take with an error, so
// the adapter refuses it by name before spending anything, per model. The
// controls are a request without an effort to the same model, the same effort
// on a model xAI documents it for (which reaches the wire in xAI's field), and
// the same line through OpenRouter, whose efforts are not xAI's to state.
func TestXAIRefusesAnEffortItsModelDoesNotTakeBeforeSpendingAnything(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(upstream.Close)
	baseURL, client := upstream.URL, (*http.Client)(nil)
	if reviewed, bound := identityBoundTestBaseURL("xai"); bound {
		baseURL, client = reviewed, identityBoundFakeClient(t, upstream.URL)
	}
	adapter, err := New(Config{Provider: "xai", BaseURL: baseURL, Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: client})
	if err != nil {
		t.Fatalf("building xai: %v", err)
	}
	route := func(id string) provider.Route {
		return provider.Route{DeploymentID: "dep_xai", Provider: "xai", ModelReference: contract.ModelReference("x-ai/" + id + "@observed-2026-09-30"), UpstreamModelID: id}
	}

	for _, id := range []string{"grok-build-0.1", "grok-4.20-0309-reasoning", "grok-4.20-0309-non-reasoning", "grok-4.20-multi-agent-0309", "grok-9-unreviewed"} {
		for _, effort := range contract.ReasoningEfforts() {
			_, err := adapter.Translate(reasoningRequest(effort), route(id))
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Code != contract.CodeInvalidRequest || unsupported.Param != "reasoning.effort" {
				t.Errorf("xai/%s effort %s: want invalid_request naming reasoning.effort, got %v", id, effort, err)
			}
		}
		if _, err := adapter.Translate(reasoningRequest(""), route(id)); err != nil {
			t.Errorf("control: xai/%s without an effort was refused: %v", id, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("a refused effort reached xAI %d times", calls.Load())
	}

	for _, id := range []string{"grok-4.7", "grok-4.6", "grok-4.5", "grok-4.3"} {
		for _, effort := range contract.ReasoningEfforts() {
			call, err := adapter.Translate(reasoningRequest(effort), route(id))
			if err != nil {
				t.Fatalf("control: xai/%s effort %s was refused: %v", id, effort, err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(call.Body, &wire); err != nil {
				t.Fatal(err)
			}
			if string(wire["reasoning_effort"]) != `"`+string(effort)+`"` {
				t.Errorf("xai/%s: reasoning_effort = %s, want %q", id, wire["reasoning_effort"], effort)
			}
		}
	}

	openrouter := sendThroughRealWireFor(t, "openrouter", "x-ai/grok-build-0.1", reasoningRequest(contract.ReasoningEffortHigh))
	if string(openrouter["reasoning"]) != `{"effort":"high"}` {
		t.Errorf("control: openrouter/x-ai/grok-build-0.1 reasoning = %s", openrouter["reasoning"])
	}
}
