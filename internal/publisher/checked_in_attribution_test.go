package publisher

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// A model nobody attributed is DROPPED and named — never guessed. That is the
// right behaviour and it is also the quietest possible failure: the publisher
// runs, every job is green, and the snapshot it writes is missing rows nobody
// asked for. A provider with no entry at all publishes ZERO models this way.
//
// So this crosses the two checked-in files against each other: every deployment
// the inventory declares must have an attribution entry, and that entry must
// name the same model line the inventory does. Neither file can drift alone.
func TestCheckedInAttributionCoversTheCheckedInInventory(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}

	raw, err := os.ReadFile("../../configs/inventory.json")
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	var file struct {
		Deployments []struct {
			DeploymentID    string `json:"deploymentId"`
			Provider        string `json:"provider"`
			ModelReference  string `json:"modelReference"`
			UpstreamModelID string `json:"upstreamModelId"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse inventory: %v", err)
	}

	// The vacuity floor. An emptied inventory, or a shape this stopped matching,
	// would satisfy every assertion below over nothing.
	if len(file.Deployments) < 100 {
		t.Fatalf("parsed %d deployments, want at least 100", len(file.Deployments))
	}

	var covered int
	for _, d := range file.Deployments {
		line := contract.ModelID(strings.SplitN(d.ModelReference, "@", 2)[0])

		// `stealth/` is OpenRouter's placeholder for a release whose publisher is
		// undisclosed. It is deliberately unattributed, so the publisher drops it
		// and names it rather than asserting a publisher nobody knows.
		if strings.HasPrefix(string(line), "stealth/") {
			if _, attributed := table.ModelLine(contract.ProviderSlug(d.Provider), d.UpstreamModelID); attributed {
				t.Errorf("%s: %q is attributed, but `stealth/` means the publisher is undisclosed", d.DeploymentID, line)
			}
			continue
		}

		got, attributed := table.ModelLine(contract.ProviderSlug(d.Provider), d.UpstreamModelID)
		if !attributed {
			t.Errorf("%s: %s/%q is in the inventory and in no attribution entry — the publisher would drop it", d.DeploymentID, d.Provider, d.UpstreamModelID)
			continue
		}
		if got != line {
			t.Errorf("%s: attribution says %q, inventory says %q", d.DeploymentID, got, line)
			continue
		}
		covered++
	}

	if covered < 100 {
		t.Fatalf("only %d deployments were positively covered", covered)
	}
	t.Logf("%d of %d deployments attributed and agreeing", covered, len(file.Deployments))
}

func TestCheckedInAttributionCanStillRefuse(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	// Negative controls: without these, a table that answered everything would
	// pass the assertions above while measuring nothing.
	if _, ok := table.ModelLine("openrouter", "definitely-not/a-model"); ok {
		t.Error("an unattributed id resolves")
	}
	if _, ok := table.ModelLine("definitely-not-a-provider", "google/gemma-4-31b-it"); ok {
		t.Error("an unknown provider resolves")
	}
	if _, ok := table.ModelLine("openrouter", "stealth/ox-alpha"); ok {
		t.Error("stealth/ox-alpha is attributed, and its publisher is undisclosed")
	}
}

func TestExternalRadarDoesNotCreateGatewayOrUnverifiedProviderAttribution(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}

	// A model exposed by a gateway can still appear under an already reviewed
	// direct provider, so this assertion is intentionally about provider blocks,
	// not substrings in model ids. None of these candidates has a direct,
	// authenticated and immutable provider catalogue Kaana can publish today.
	for _, slug := range []contract.ProviderSlug{
		"amd-radeon", "requesty", "vercel-ai-gateway", "huggingface", "ollama-cloud",
		"kilo-code", "opencode-zen", "aion-labs", "agnes-ai", "glhf", "ollama",
	} {
		if models, attributed := table.byProvider[slug]; attributed {
			t.Errorf("excluded radar provider %q has %d checked-in model attributions", slug, len(models))
		}
	}
}

func TestCheckedInAttributionClassifiesTheLiveCatalogueDelta(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	if got := len(table.byProvider["openrouter"]); got != 347 {
		t.Fatalf("OpenRouter attributions = %d, want the 347 entries whose provenance the checked-in file declares", got)
	}

	supported := map[contract.ProviderSlug]map[string]contract.ModelID{
		"openrouter": {
			"ibm-granite/granite-4.2-8b":          "ibm-granite/granite-4.2-8b",
			"inclusionai/ling-3.0-flash-fin:free": "inclusionai/ling-3.0-flash-fin",
			"minimax/minimax-m2.7:free":           "minimax/minimax-m2.7",
			"minimax/minimax-m3:free":             "minimax/minimax-m3",
			"mistralai/devstral-2512":             "mistralai/devstral-2512",
			"qwen/qwen3.8-flash":                  "qwen/qwen3.8-flash",
			"tencent/hy-mt2-7b":                   "tencent/hy-mt2-7b",
			"thinkingmachines/inkling-small:free": "thinkingmachines/inkling-small",
			"thinkingmachines/inkling:free":       "thinkingmachines/inkling",
			"z-ai/glm-5.3-flash":                  "z-ai/glm-5.3-flash",
		},
	}
	for slug, models := range supported {
		for upstreamModelID, want := range models {
			got, ok := table.ModelLine(slug, upstreamModelID)
			if !ok {
				t.Errorf("%s/%s is absent", slug, upstreamModelID)
				continue
			}
			if got != want {
				t.Errorf("%s/%s = %q, want %q", slug, upstreamModelID, got, want)
			}
		}
	}

	for slug, models := range table.byProvider {
		for upstreamModelID := range models {
			switch {
			case strings.HasPrefix(upstreamModelID, "~"):
				t.Errorf("%s/%s attributes a moving alias", slug, upstreamModelID)
			case strings.HasSuffix(strings.SplitN(upstreamModelID, ":", 2)[0], "-latest"):
				t.Errorf("%s/%s attributes a moving alias", slug, upstreamModelID)
			case strings.HasPrefix(upstreamModelID, "openrouter/"):
				t.Errorf("%s/%s attributes a router rather than one model", slug, upstreamModelID)
			case strings.HasSuffix(upstreamModelID, ":batch"), strings.HasSuffix(upstreamModelID, ":thinking"):
				t.Errorf("%s/%s attributes a delivery mode rather than weights", slug, upstreamModelID)
			case slug == "groq" && (strings.HasPrefix(upstreamModelID, "canopylabs/orpheus-") || strings.HasPrefix(upstreamModelID, "whisper-") || strings.HasPrefix(upstreamModelID, "groq/compound")):
				t.Errorf("%s/%s cannot produce the chat contract Kaana serves", slug, upstreamModelID)
			case slug == "xai" && strings.HasPrefix(upstreamModelID, "grok-imagine-"):
				t.Errorf("%s/%s cannot produce the chat contract Kaana serves", slug, upstreamModelID)
			}
		}
	}
}

func TestCheckedInAttributionPinsOnlyTheDocumentedNebiusAndNscaleExamples(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}

	want := map[contract.ProviderSlug]map[string]contract.ModelID{
		"nebius": {
			"meta-llama/Meta-Llama-3.1-70B-Instruct": "meta-llama/llama-3.1-70b-instruct",
			"meta-llama/Llama-3.3-70B-Instruct":      "meta-llama/llama-3.3-70b-instruct",
		},
		"nscale": {
			"meta-llama/Llama-3.1-8B-Instruct": "meta-llama/llama-3.1-8b-instruct",
		},
	}
	for slug, models := range want {
		if got := len(table.byProvider[slug]); got != len(models) {
			t.Errorf("%s has %d attributions, want exactly the %d documented examples", slug, got, len(models))
		}
		for upstreamModelID, modelLine := range models {
			got, ok := table.ModelLine(slug, upstreamModelID)
			if !ok || got != modelLine {
				t.Errorf("%s/%s = %q, %t; want %q", slug, upstreamModelID, got, ok, modelLine)
			}
		}
	}

	for _, candidate := range []struct {
		slug contract.ProviderSlug
		id   string
	}{
		{slug: "nebius", id: "meta-llama/Meta-Llama-3.1-70B-Instruct-fast"},
		{slug: "nscale", id: "default"},
	} {
		if _, ok := table.ModelLine(candidate.slug, candidate.id); ok {
			t.Errorf("%s/%s attributes a delivery alias rather than fixed weights", candidate.slug, candidate.id)
		}
	}
}

func TestCheckedInAttributionPinsOnlyDocumentedAlibabaSnapshots(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	want := map[string]contract.ModelID{
		"qwen3.6-flash-2026-04-16": "qwen/qwen3.6-flash-2026-04-16",
		"qwen3.6-plus-2026-04-02":  "qwen/qwen3.6-plus-2026-04-02",
		"qwen3.7-flash-2026-07-15": "qwen/qwen3.7-flash-2026-07-15",
		"qwen3.7-plus-2026-05-26":  "qwen/qwen3.7-plus-2026-05-26",
	}
	if got := len(table.byProvider["alibaba"]); got != len(want) {
		t.Fatalf("Alibaba has %d attributions, want exactly %d dated snapshots", got, len(want))
	}
	for upstreamModelID, modelLine := range want {
		got, ok := table.ModelLine("alibaba", upstreamModelID)
		if !ok || got != modelLine {
			t.Errorf("alibaba/%s = %q, %t; want %q", upstreamModelID, got, ok, modelLine)
		}
	}
	for _, excluded := range []string{"qwen3.7-plus", "qwen3.7-flash", "qwen3.8-max", "qwen3.7-max-preview"} {
		if _, ok := table.ModelLine("alibaba", excluded); ok {
			t.Errorf("alibaba/%s attributes a moving or preview id", excluded)
		}
	}
}
