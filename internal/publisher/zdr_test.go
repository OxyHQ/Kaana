package publisher

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// openRouterProductionModels reproduces the 2026-09-30 production failure
// class from OpenRouter's public lists: every model's own `/models` entry
// lists `temperature`, but gemini-3.7-flash's zero-retention endpoints do
// not, gpt-4o-mini's list only `max_completion_tokens`, and qwen3.8-flash has
// no zero-retention endpoint at all.
const openRouterProductionModels = `{"data":[
 {"id":"google/gemini-3.7-flash","canonical_slug":"google/gemini-3.7-flash-20260813","name":"Google: Gemini 3.7 Flash",
  "supported_parameters":["include_reasoning","max_tokens","reasoning","reasoning_effort","response_format","seed","stop","structured_outputs","temperature","tool_choice","tools","top_p"]},
 {"id":"openai/gpt-4o-mini","canonical_slug":"openai/gpt-4o-mini","name":"OpenAI: GPT-4o-mini",
  "supported_parameters":["frequency_penalty","logit_bias","logprobs","max_completion_tokens","max_tokens","presence_penalty","response_format","seed","stop","structured_outputs","temperature","tool_choice","tools","top_logprobs","top_p"]},
 {"id":"qwen/qwen3.8-flash","canonical_slug":"qwen/qwen3.8-flash-20260826","name":"Qwen: Qwen3.8 Flash",
  "supported_parameters":["frequency_penalty","max_tokens","presence_penalty","reasoning","response_format","seed","stop","temperature","tool_choice","tools","top_p"]}
]}`

const openRouterProductionZDR = `{"data":[
 {"name":"Google | google/gemini-3.7-flash-20260813","model_id":"google/gemini-3.7-flash","provider_name":"Google","tag":"google-vertex",
  "supported_parameters":["reasoning","include_reasoning","max_tokens","seed","response_format","stop","structured_outputs","tools","tool_choice","reasoning_effort"],"status":0},
 {"name":"Google | google/gemini-3.7-flash-20260813","model_id":"google/gemini-3.7-flash","provider_name":"Google","tag":"google-vertex/global",
  "supported_parameters":["reasoning","include_reasoning","max_tokens","seed","response_format","stop","structured_outputs","tools","tool_choice","reasoning_effort"],"status":0},
 {"name":"Azure | openai/gpt-4o-mini","model_id":"openai/gpt-4o-mini","provider_name":"Azure","tag":"azure",
  "supported_parameters":["max_completion_tokens","temperature","top_p","stop","frequency_penalty","presence_penalty","web_search_options","seed","logit_bias","logprobs","top_logprobs","response_format","structured_outputs"],"status":0}
]}`

func TestOpenRouterParametersAreWhatItsZeroRetentionEndpointsAccept(t *testing.T) {
	server := serveOpenRouter(t, openRouterProductionModels, openRouterProductionZDR)
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byID := map[string]DiscoveredModel{}
	for _, model := range models {
		byID[model.UpstreamModelID] = model
	}

	gemini := byID["google/gemini-3.7-flash"]
	if gemini.Unservable != "" || gemini.Observed == nil || gemini.Observed.AcceptedParameters == nil {
		t.Fatalf("gemini = %+v", gemini)
	}
	accepted := *gemini.Observed.AcceptedParameters
	if slices.Contains(accepted, provider.ParameterTemperature) || slices.Contains(accepted, provider.ParameterTopP) {
		t.Errorf("gemini's /models entry lists temperature and top_p, its zero-retention endpoints do not, and the route was published accepting them: %v", accepted)
	}
	if !slices.Contains(accepted, provider.ParameterTools) || !slices.Contains(accepted, provider.ParameterMaxOutputTokens) {
		t.Errorf("gemini's zero-retention endpoints accept tools and max_tokens: %v", accepted)
	}

	mini := byID["openai/gpt-4o-mini"].Observed
	if mini == nil || mini.AcceptedParameters == nil {
		t.Fatalf("gpt-4o-mini observed = %+v", mini)
	}
	if !slices.Contains(*mini.AcceptedParameters, provider.ParameterMaxOutputTokens) || !slices.Contains(*mini.AcceptedParameters, provider.ParameterTemperature) {
		t.Errorf("max_completion_tokens is the same control as max_tokens, and temperature is listed: %v", *mini.AcceptedParameters)
	}
	// Its zero-retention endpoints list no tools, though the model list does:
	// every field that list decides follows the reachable endpoints.
	if mini.SupportsTools == nil || *mini.SupportsTools || slices.Contains(*mini.AcceptedParameters, provider.ParameterTools) {
		t.Errorf("gpt-4o-mini's zero-retention endpoints take no tools: tools %v params %v", mini.SupportsTools, *mini.AcceptedParameters)
	}
	if mini.ReasoningEfforts == nil || len(*mini.ReasoningEfforts) != 0 {
		t.Errorf("gpt-4o-mini's zero-retention endpoints take no reasoning control: %v", mini.ReasoningEfforts)
	}

	if qwen := byID["qwen/qwen3.8-flash"]; qwen.Unservable == "" {
		t.Errorf("a model with no zero-retention endpoint was not marked unservable: %+v", qwen)
	}

	// Positive control: the very same model list read as a provider with no
	// zero-retention policy keeps its own statement, so it is the zero-retention
	// list — not the mapping — that removed temperature above.
	plain := serveModelList(t, openRouterProductionModels)
	controls, err := Discover(context.Background(), plain.Client(), Provider{Slug: "custom-compatible", BaseURL: plain.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("control Discover: %v", err)
	}
	for _, model := range controls {
		if model.Unservable != "" || model.Observed == nil || model.Observed.AcceptedParameters == nil ||
			!slices.Contains(*model.Observed.AcceptedParameters, provider.ParameterTemperature) {
			t.Errorf("control: %s lost what its own list says: %+v", model.UpstreamModelID, model)
		}
	}
}

func TestAnOpenRouterModelWithNoZeroRetentionEndpointIsDroppedAndNamed(t *testing.T) {
	server := serveOpenRouter(t, openRouterProductionModels, openRouterProductionZDR)
	attribution, err := ParseAttribution([]byte(`{"attribution":{"openrouter":{
	  "google/gemini-3.7-flash":"google/gemini-3.7-flash",
	  "qwen/qwen3.8-flash":"qwen/qwen3.8-flash"}}}`))
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	var logs bytes.Buffer
	store := &fakeStore{}
	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"}},
		Attribution: attribution,
		Store:       store,
		Client:      server.Client(),
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	published := parseSnapshot(t, store.written()[0])
	ids := make([]string, 0, len(published.Deployments))
	for _, deployment := range published.Deployments {
		ids = append(ids, deployment.UpstreamModelID)
	}
	// Positive control: the attributed model that HAS a zero-retention
	// endpoint is published, so the drop below is not an empty snapshot.
	if !slices.Equal(ids, []string{"google/gemini-3.7-flash"}) {
		t.Errorf("published %v, want only the model with a zero-retention endpoint", ids)
	}
	if !strings.Contains(logs.String(), "openrouter/qwen/qwen3.8-flash") || !strings.Contains(logs.String(), "zero-data-retention") {
		t.Errorf("the drop was not warned about by name:\n%s", logs.String())
	}
}

func TestOpenRouterDiscoveryFailsClosedWithoutAReadableZeroRetentionList(t *testing.T) {
	for name, zdr := range map[string]string{
		"an empty list":                 `{"data":[]}`,
		"an endpoint without model_id":  `{"data":[{"provider_name":"Azure","supported_parameters":["temperature"]}]}`,
		"a body that is not the list":   `{"data":{"model_id":"x"}}`,
		"not JSON":                      `<html>`,
		"a list the server cannot find": "",
	} {
		t.Run(name, func(t *testing.T) {
			server := serveOpenRouter(t, openRouterProductionModels, zdr)
			if _, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"}); err == nil {
				t.Fatal("OpenRouter was discovered without a readable zero-retention list")
			}
		})
	}
	// Control: the same model list with a readable zero-retention list succeeds.
	server := serveOpenRouter(t, openRouterProductionModels, openRouterProductionZDR)
	if _, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"}); err != nil {
		t.Fatalf("control: %v", err)
	}
}

func TestOnlyOpenRouterIsAskedForTheZeroRetentionList(t *testing.T) {
	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/endpoints/zdr" {
			asked.Add(1)
			_, _ = w.Write([]byte(openRouterProductionZDR))
			return
		}
		_, _ = w.Write([]byte(openRouterProductionModels))
	}))
	t.Cleanup(server.Close)

	if _, err := Discover(context.Background(), server.Client(), Provider{Slug: "custom-compatible", BaseURL: server.URL + "/v1", APIKey: "k"}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if asked.Load() != 0 {
		t.Errorf("a provider without OpenRouter's policy was asked for OpenRouter's zero-retention list")
	}
	// Control: OpenRouter is asked, exactly once per discovery.
	if _, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if asked.Load() != 1 {
		t.Errorf("OpenRouter's zero-retention list was read %d times, want 1", asked.Load())
	}
}

func TestEveryRequestParameterHasAProviderWord(t *testing.T) {
	if len(providerParameterWords) != len(provider.RequestParameters()) {
		t.Errorf("%d mapped controls for a vocabulary of %d", len(providerParameterWords), len(provider.RequestParameters()))
	}
	for _, parameter := range provider.RequestParameters() {
		if len(providerParameterWords[parameter]) == 0 {
			t.Errorf("%s has no provider word, so no route could ever be published accepting it", parameter)
		}
	}
}

// openRouterToolChoiceModels and openRouterToolChoiceZDR are OpenRouter's
// public lists as captured on 2026-10-12, trimmed. Claude Opus 5.5's
// zero-retention endpoints list `tool_choice` and state, value by value, that
// they take `none` and `auto` but not a forced choice; the second model's only
// endpoint takes `auto` alone; the third's endpoints state nothing per value.
const openRouterToolChoiceModels = `{"data":[
 {"id":"anthropic/claude-opus-5.5","canonical_slug":"anthropic/claude-opus-5.5-20260921","name":"Anthropic: Claude Opus 5.5",
  "supported_parameters":["include_reasoning","max_completion_tokens","max_tokens","reasoning","reasoning_effort","response_format","stop","structured_outputs","tool_choice","tools","verbosity"]},
 {"id":"example/auto-only","name":"Example: Auto Only","supported_parameters":["max_tokens","tool_choice","tools"]},
 {"id":"example/unstated","name":"Example: Unstated","supported_parameters":["max_tokens","tool_choice","tools"]}
]}`

const openRouterToolChoiceZDR = `{"data":[
 {"name":"Amazon Bedrock | anthropic/claude-opus-5.5-20260921","model_id":"anthropic/claude-opus-5.5","provider_name":"Amazon Bedrock","tag":"amazon-bedrock/eu-west-1",
  "supported_parameters":["reasoning","include_reasoning","max_tokens","stop","tool_choice","tools","response_format","verbosity","reasoning_effort"],
  "supports_tool_choice":{"none":true,"auto":true,"required":false,"function":false},"status":0},
 {"name":"Google | anthropic/claude-opus-5.5-20260921","model_id":"anthropic/claude-opus-5.5","provider_name":"Google","tag":"google-vertex/global",
  "supported_parameters":["reasoning","include_reasoning","max_tokens","stop","tool_choice","tools","response_format","verbosity","reasoning_effort"],
  "supports_tool_choice":{"none":true,"auto":true,"required":false,"function":false},"status":0},
 {"name":"Example | example/auto-only","model_id":"example/auto-only","provider_name":"Example","tag":"example",
  "supported_parameters":["max_tokens","tool_choice","tools"],
  "supports_tool_choice":{"none":false,"auto":true,"required":false,"function":false},"status":0},
 {"name":"Example | example/unstated","model_id":"example/unstated","provider_name":"Example","tag":"example",
  "supported_parameters":["max_tokens","tool_choice","tools"],"status":0},
 {"name":"Other | example/unstated","model_id":"example/unstated","provider_name":"Other","tag":"other",
  "supported_parameters":["max_tokens","tools"],"supports_tool_choice":{"none":true,"auto":"yes","required":false,"function":false},"status":0}
]}`

// TestOpenRouterToolChoiceValuesAreWhatItsZeroRetentionEndpointsState: a
// forced choice on Claude Opus 5.5 is not published as accepted, because no
// reachable endpoint takes it, while `none` is; `auto` needs no word at all.
func TestOpenRouterToolChoiceValuesAreWhatItsZeroRetentionEndpointsState(t *testing.T) {
	server := serveOpenRouter(t, openRouterToolChoiceModels, openRouterToolChoiceZDR)
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	accepted := map[string][]provider.RequestParameter{}
	for _, model := range models {
		if model.Observed == nil || model.Observed.AcceptedParameters == nil {
			t.Fatalf("%s published no accepted set: %+v", model.UpstreamModelID, model)
		}
		accepted[model.UpstreamModelID] = *model.Observed.AcceptedParameters
	}

	opus := accepted["anthropic/claude-opus-5.5"]
	if !slices.Contains(opus, provider.ParameterToolChoice) || !slices.Contains(opus, provider.ParameterToolChoiceNone) {
		t.Errorf("Opus 5.5's endpoints take tool_choice none: %v", opus)
	}
	if slices.Contains(opus, provider.ParameterToolChoiceRequired) || slices.Contains(opus, provider.ParameterToolChoiceFunction) {
		t.Errorf("Opus 5.5's endpoints state required and function false, and a forced choice was published: %v", opus)
	}
	route := provider.Route{AcceptedParameters: &opus}
	if _, refused := route.UnacceptedParameter(provider.ToolChoiceParameters(&contract.ToolChoice{Mode: modePointer(contract.ToolChoiceAuto)})...); refused {
		t.Error("an auto tool choice was refused on a route that takes tools")
	}
	if parameter, refused := route.UnacceptedParameter(provider.ToolChoiceParameters(&contract.ToolChoice{Function: &contract.ToolChoiceFunction{Name: "lookup"}})...); !refused || parameter != provider.ParameterToolChoiceFunction {
		t.Errorf("a named-tool choice was not refused: %q %v", parameter, refused)
	}

	// An endpoint that takes auto alone states no value, and the field goes
	// with them: a set naming the field alone would read as every value.
	if autoOnly := accepted["example/auto-only"]; slices.Contains(autoOnly, provider.ParameterToolChoice) ||
		!slices.Equal(autoOnly, []provider.RequestParameter{provider.ParameterMaxOutputTokens, provider.ParameterTools}) {
		t.Errorf("auto-only = %v", autoOnly)
	}

	// Control: an endpoint listing tool_choice without a readable per-value
	// statement takes every value — the word speaks for the field and its
	// values — and the other endpoint's unreadable statement cannot narrow it.
	unstated := accepted["example/unstated"]
	for _, value := range append([]provider.RequestParameter{provider.ParameterToolChoice}, provider.ToolChoiceValues()...) {
		if !slices.Contains(unstated, value) {
			t.Errorf("unstated lacks %s: %v", value, unstated)
		}
	}
}

func modePointer(mode contract.ToolChoiceMode) *contract.ToolChoiceMode { return &mode }
