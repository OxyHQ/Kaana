package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// geminiZeroRetentionParameters is what the publisher derives for
// google/gemini-3.7-flash on OpenRouter from its zero-retention endpoints
// (2026-09-30): no temperature, no top_p.
var geminiZeroRetentionParameters = []provider.RequestParameter{
	provider.ParameterMaxOutputTokens, provider.ParameterReasoningEffort, provider.ParameterResponseFormat,
	provider.ParameterSeed, provider.ParameterStopSequences, provider.ParameterToolChoice, provider.ParameterTools,
}

// newOpenRouterAgainst builds the real OpenRouter adapter (its reserved
// origin, so its policy applies) talking to a real-wire fake.
func newOpenRouterAgainst(t *testing.T, handler http.HandlerFunc) (*Adapter, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)
	baseURL, _ := identityBoundTestBaseURL("openrouter")
	adapter, err := New(Config{
		Provider:     "openrouter",
		BaseURL:      baseURL,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey}),
		HTTPClient:   identityBoundFakeClient(t, upstream.URL),
	})
	if err != nil {
		t.Fatalf("building the OpenRouter adapter: %v", err)
	}
	return adapter, &calls
}

func openRouterGeminiRoute(accepted *[]provider.RequestParameter) provider.Route {
	return provider.Route{
		DeploymentID:       "dep_openrouter_google_gemini_3_7_flash_observed_2026_09_01",
		Provider:           "openrouter",
		ModelReference:     "google/gemini-3.7-flash@observed-2026-09-01",
		UpstreamModelID:    "google/gemini-3.7-flash",
		AcceptedParameters: accepted,
	}
}

func completedChat(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"))
}

func TestTranslateRefusesAParameterTheRouteIsKnownNotToAccept(t *testing.T) {
	adapter, calls := newOpenRouterAgainst(t, completedChat)
	accepted := append([]provider.RequestParameter(nil), geminiZeroRetentionParameters...)
	route := openRouterGeminiRoute(&accepted)

	temperature := 0.7
	maxTokens := 256
	withTemperature := requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")}}})
	withTemperature.Sampling.Temperature = &temperature
	withTemperature.MaxOutputTokens = &maxTokens

	_, err := adapter.Translate(withTemperature, route)
	var unsupported provider.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("a temperature the route's zero-retention endpoints refuse was translated: %v", err)
	}
	if unsupported.Code != contract.CodeInvalidRequest || unsupported.Code.Retryable() || unsupported.Param != "sampling.temperature" ||
		!strings.Contains(unsupported.Detail, "zero-data-retention") {
		t.Errorf("refusal = %+v", unsupported)
	}
	if calls.Load() != 0 {
		t.Errorf("a refused translation reached the upstream %d times", calls.Load())
	}

	// Control 1: the same request without temperature is translated and served
	// on the same route; maxOutputTokens is in the set and is sent.
	withoutTemperature := requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")}}})
	withoutTemperature.MaxOutputTokens = &maxTokens
	call, err := adapter.Translate(withoutTemperature, route)
	if err != nil {
		t.Fatalf("control: an accepted request was refused: %v", err)
	}
	if _, err := adapter.Stream(context.Background(), call, silentEmitter{}, nil); err != nil {
		t.Fatalf("control: the accepted request failed upstream: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("control: the upstream saw %d requests, want 1", calls.Load())
	}

	// Control 2: a route whose set is UNKNOWN refuses nothing and sends the
	// caller's temperature exactly — absence is never "accepts nothing".
	call, err = adapter.Translate(withTemperature, openRouterGeminiRoute(nil))
	if err != nil {
		t.Fatalf("control: an unknown set refused a request: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &wire); err != nil || string(wire["temperature"]) != "0.7" {
		t.Errorf("control: the caller's temperature was not sent as given: %s", wire["temperature"])
	}

	// Control 3: the check reads what is SENT. A `text` response format sends
	// nothing, so a route without responseFormat does not refuse it; a
	// json_object one is sent and is refused.
	none := []provider.RequestParameter{}
	text := requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")}}})
	text.ResponseFormat = &contract.ResponseFormat{Type: contract.ResponseFormatText}
	if _, err := adapter.Translate(text, openRouterGeminiRoute(&none)); err != nil {
		t.Errorf("a response format that sends nothing was refused: %v", err)
	}
	text.ResponseFormat = &contract.ResponseFormat{Type: contract.ResponseFormatJSONObject}
	if _, err := adapter.Translate(text, openRouterGeminiRoute(&none)); !errors.As(err, &unsupported) || unsupported.Param != "responseFormat" {
		t.Errorf("a json_object format on a route that takes none was not refused: %v", err)
	}
}

// openRouterNotFound answers the way OpenRouter does: HTTP 404 with its
// documented envelope, `{"error":{"code":404,"message":...}}`.
func openRouterNotFound(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 404, "message": message}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}
}

func TestOpenRouterPolicyNotFoundIsARequestRefusalAndAbsenceStaysModelNotFound(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		message string
		want    contract.ErrorCode
	}{
		{"require_parameters", "No endpoints found that can handle the requested parameters. To learn more about provider routing, visit: https://openrouter.ai/docs/guides/routing/provider-selection", contract.CodeInvalidRequest},
		{"tool use", "No endpoints found that support tool use. To learn more about provider routing, visit: https://openrouter.ai/docs/guides/routing/provider-selection", contract.CodeInvalidRequest},
		{"tool_choice", "No endpoints found that support the provided 'tool_choice' value.", contract.CodeInvalidRequest},
		{"zero data retention", "No endpoints found matching your data policy (Zero data retention). Configure: https://openrouter.ai/settings/privacy", contract.CodeInvalidRequest},
		{"guardrail and data policy", "No endpoints available matching your guardrail restrictions and data policy. Configure: https://openrouter.ai/settings/privacy", contract.CodeInvalidRequest},
		// An echoed credential is removed by exact match; the classification
		// and the rest of the diagnostic survive.
		{"echoed credential", "No endpoints found that can handle the requested parameters. key " + fakeAPIKey, contract.CodeInvalidRequest},
		// Controls: true absence keeps its meaning.
		{"model absent", "No endpoints found for qwen/qwen3.8-flash.", contract.CodeModelNotFound},
		{"no message", "", contract.CodeModelNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			adapter, calls := newOpenRouterAgainst(t, openRouterNotFound(testCase.message))
			call, err := adapter.Translate(requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")}}}), openRouterGeminiRoute(nil))
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
			var upstream provider.ErrUpstream
			if !errors.As(streamErr, &upstream) {
				t.Fatalf("stream error = %v", streamErr)
			}
			if upstream.Code != testCase.want {
				t.Fatalf("code = %s (%s), want %s", upstream.Code, upstream.Detail, testCase.want)
			}
			if provider.AttributableCategory(upstream.Category) {
				t.Errorf("category %s is attributable: one caller's parameters would trip the breaker", upstream.Category)
			}
			contractError := upstream.ContractError("req")
			if contractError.Retryable {
				t.Error("an identical retry meets the same filter, yet the error is retryable")
			}
			if calls.Load() != 1 {
				t.Errorf("the upstream saw %d requests, want 1", calls.Load())
			}
			if testCase.message != "" {
				if upstream.Passthrough == nil || upstream.Passthrough.Message == nil {
					t.Fatalf("OpenRouter's diagnostic was lost: %+v", upstream.Passthrough)
				}
				message := *upstream.Passthrough.Message
				if strings.Contains(message, fakeAPIKey) {
					t.Errorf("the credential reached the passthrough: %s", message)
				}
				if !strings.HasPrefix(message, testCase.message[:20]) {
					t.Errorf("the passthrough lost OpenRouter's words: %q", message)
				}
			}
			// The request, not the key, was at fault: the key stays usable.
			if usable := adapter.credentials.Projection(time.Now()).Usable; usable != 1 {
				t.Errorf("a request-level 404 took the key out of rotation (%d usable)", usable)
			}
		})
	}
}

// TestThePolicyNotFoundReadingIsOpenRoutersAlone: the words are OpenRouter's,
// so another provider answering 404 with them keeps model_not_found.
func TestThePolicyNotFoundReadingIsOpenRoutersAlone(t *testing.T) {
	upstream := httptest.NewServer(openRouterNotFound("No endpoints found that can handle the requested parameters."))
	t.Cleanup(upstream.Close)
	adapter, err := New(Config{Provider: "custom-compatible", BaseURL: upstream.URL, Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	route := openRouterGeminiRoute(nil)
	route.Provider = "custom-compatible"
	call, err := adapter.Translate(requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")}}}), route)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
	var failure provider.ErrUpstream
	if !errors.As(streamErr, &failure) || failure.Code != contract.CodeModelNotFound {
		t.Errorf("another provider's 404 was read with OpenRouter's vocabulary: %v", streamErr)
	}
}

// TestTheSpokenWireIsJudgedByTheSameAcceptedSet: the spoken path builds its
// own body, and a route's accepted set refuses from it exactly as from a text
// body. `modalities` and `audio` are not caller controls the set states, so a
// set without any sampling control still translates a plain spoken request.
func TestTheSpokenWireIsJudgedByTheSameAcceptedSet(t *testing.T) {
	adapter, _ := newOpenRouterAgainst(t, completedChat)
	none := []provider.RequestParameter{}
	route := openRouterGeminiRoute(&none)
	route.UpstreamModelID = "openai/gpt-audio-mini"

	if _, err := adapter.Translate(spokenRequest(contract.ModalityAudio), route); err != nil {
		t.Fatalf("control: a spoken request carrying no sampling control was refused: %v", err)
	}
	temperature := 0.7
	spoken := spokenRequest(contract.ModalityAudio)
	spoken.Sampling.Temperature = &temperature
	_, err := adapter.Translate(spoken, route)
	var unsupported provider.ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Param != "sampling.temperature" {
		t.Errorf("a spoken temperature the route does not accept was translated: %v", err)
	}
}

// TestEveryRequestParameterHasAWireField keeps the vocabulary and the wire in
// step: a control added to the vocabulary with no body field here could never
// be refused, whatever a route's set said.
func TestEveryRequestParameterHasAWireField(t *testing.T) {
	covered := map[provider.RequestParameter]bool{}
	for _, parameter := range wireParameters {
		covered[parameter] = true
	}
	for _, parameter := range provider.RequestParameters() {
		if !covered[parameter] {
			t.Errorf("%s has no wire field, so Translate can never refuse it", parameter)
		}
	}
}
