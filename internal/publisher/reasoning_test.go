package publisher

import (
	"context"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// xAI's `GET /v1/models` shape (captured fields only): it says nothing about
// reasoning efforts.
const xAIModelsFixture = `{"object":"list","data":[
  {"id":"grok-4.5","object":"model","created":1780000000,"owned_by":"xai"},
  {"id":"grok-build-0.1","object":"model","created":1780000000,"owned_by":"xai"}
]}`

// OpenRouter lists `reasoning` for both lines, which Kaana reads as the whole
// normalized effort vocabulary.
const openRouterGrokFixture = `{"data":[
  {"id":"x-ai/grok-4.5","name":"xAI: Grok 4.5","created":1780000000,"supported_parameters":["reasoning","reasoning_effort","max_tokens","tools"]},
  {"id":"x-ai/grok-build-0.1","name":"xAI: Grok Build 0.1","created":1780000000,"supported_parameters":["reasoning","include_reasoning","max_tokens","tools"]}
]}`

const openRouterGrokZDRFixture = `{"data":[
  {"model_id":"x-ai/grok-4.5","supported_parameters":["reasoning","reasoning_effort","max_tokens","tools"]},
  {"model_id":"x-ai/grok-build-0.1","supported_parameters":["reasoning","include_reasoning","max_tokens","tools"]}
]}`

// The catalogue intersects efforts across a line's deployments, and an xAI
// deployment whose model list said nothing used to abstain — so the line
// advertised OpenRouter's vocabulary to requests Oxy may sign onto the xAI
// route, which xAI rejects. The xAI deployment now carries the adapter's own
// per-model statement, and the line narrows to what every route accepts.
func TestTheXAIRouteNarrowsALineToTheEffortsXAIAccepts(t *testing.T) {
	attribution, err := ParseAttribution([]byte(`{"attribution":{
	  "openrouter":{"x-ai/grok-4.5":"x-ai/grok-4.5","x-ai/grok-build-0.1":"x-ai/grok-build-0.1"},
	  "xai":{"grok-4.5":"x-ai/grok-4.5","grok-build-0.1":"x-ai/grok-build-0.1"}}}`))
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	discover := func(slug contract.ProviderSlug, models, zdr string) Discovery {
		server := serveOpenRouter(t, models, zdr)
		found, err := Discover(context.Background(), server.Client(), Provider{Slug: slug, BaseURL: server.URL + "/v1", APIKey: "k"})
		if err != nil {
			t.Fatalf("Discover %s: %v", slug, err)
		}
		return Discovery{Provider: Provider{Slug: slug}, Models: found}
	}
	openrouter := discover("openrouter", openRouterGrokFixture, openRouterGrokZDRFixture)
	xai := discover("xai", xAIModelsFixture, "")

	efforts := func(discoveries ...Discovery) map[contract.ModelID]*[]contract.ReasoningEffort {
		t.Helper()
		built, err := BuildSnapshot(discoveries, attribution, nil, time.Now())
		if err != nil {
			t.Fatalf("BuildSnapshot: %v", err)
		}
		loaded, err := inventory.Parse(built.Body, inventory.DefaultMaxSnapshotAge)
		if err != nil {
			t.Fatalf("the reader refused the snapshot: %v", err)
		}
		lines := make(map[contract.ModelID]*[]contract.ReasoningEffort)
		for _, entry := range loaded.Catalogue() {
			lines[entry.Model] = entry.ReasoningEfforts
		}
		return lines
	}

	// Control: OpenRouter alone advertises every effort on both lines, which
	// is what the catalogue said before the xAI route stated its own.
	alone := efforts(openrouter)
	for _, line := range []contract.ModelID{"x-ai/grok-4.5", "x-ai/grok-build-0.1"} {
		if got := alone[line]; got == nil || len(*got) != len(contract.ReasoningEfforts()) {
			t.Fatalf("control: OpenRouter alone gives %s efforts %v", line, got)
		}
	}

	both := efforts(openrouter, xai)
	if got := both["x-ai/grok-build-0.1"]; got == nil || len(*got) != 0 {
		t.Errorf("x-ai/grok-build-0.1 efforts = %v, want [] (xAI documents none for it)", got)
	}
	if got := both["x-ai/grok-4.5"]; got == nil || len(*got) != len(contract.ReasoningEfforts()) {
		t.Errorf("x-ai/grok-4.5 efforts = %v, want low, medium, high", got)
	}

	// The xAI deployment's own observation is the stated list, not absence.
	for _, model := range xai.Models {
		if model.Observed == nil || model.Observed.ReasoningEfforts == nil {
			t.Errorf("xai/%s carries no effort statement", model.UpstreamModelID)
		}
	}
}
