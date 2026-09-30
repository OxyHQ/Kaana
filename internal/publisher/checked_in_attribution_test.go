package publisher

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
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

func TestOpenAIAttributionPublishesOnlyReviewedChatModels(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}

	want := map[string]contract.ModelID{
		"gpt-6.1-sol":             "openai/gpt-6.1-sol",
		"gpt-6-astra":             "openai/gpt-6-astra",
		"gpt-6-sol":               "openai/gpt-6-sol",
		"gpt-6-luna":              "openai/gpt-6-luna",
		"gpt-5.6-luna":            "openai/gpt-5.6-luna",
		"gpt-5.6-sol":             "openai/gpt-5.6-sol",
		"gpt-5.6-terra":           "openai/gpt-5.6-terra",
		"gpt-5.5-2026-04-23":      "openai/gpt-5.5-2026-04-23",
		"gpt-5.4-2026-03-05":      "openai/gpt-5.4-2026-03-05",
		"gpt-5.4-mini-2026-03-17": "openai/gpt-5.4-mini-2026-03-17",
		"gpt-5.4-nano-2026-03-17": "openai/gpt-5.4-nano-2026-03-17",
		"gpt-5.2-2025-12-11":      "openai/gpt-5.2-2025-12-11",
		"gpt-5.1-2025-11-13":      "openai/gpt-5.1-2025-11-13",
		"gpt-4.1-2025-04-14":      "openai/gpt-4.1-2025-04-14",
		"gpt-4.1-mini-2025-04-14": "openai/gpt-4.1-mini-2025-04-14",
		"gpt-4o-2024-08-06":       "openai/gpt-4o-2024-08-06",
		"gpt-4o-2024-11-20":       "openai/gpt-4o-2024-11-20",
		"gpt-4o-mini-2024-07-18":  "openai/gpt-4o-mini-2024-07-18",
	}
	if got := len(table.byProvider["openai"]); got != len(want) {
		t.Fatalf("OpenAI has %d direct attributions, want exactly %d reviewed chat models", got, len(want))
	}
	for upstreamModelID, modelLine := range want {
		got, ok := table.ModelLine("openai", upstreamModelID)
		if !ok || got != modelLine {
			t.Errorf("openai/%s = %q, %t; want %q", upstreamModelID, got, ok, modelLine)
		}
	}

	for _, excluded := range []string{
		"gpt-5.6",
		"gpt-realtime-2.1",
		"gpt-realtime-2.1-mini",
		"gpt-realtime-2",
		"gpt-realtime-translate",
		"gpt-live-transcribe",
		"gpt-realtime-whisper",
		"gpt-realtime-1.5",
		"gpt-audio-1.5",
		// Undated aliases that re-point to a newer snapshot: the dated id is
		// attributed instead.
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-4.1",
		"gpt-4o",
		"gpt-4o-mini",
		"chat-latest",
		// Responses-only or separately gated: no Chat Completions route.
		"gpt-5.5-pro",
		"gpt-5.4-pro",
		"gpt-5.3-codex",
		"gpt-5.6-cyber",
		"gpt-oss-120b",
		"gpt-rosalind-research",
		// Snapshots OpenAI has scheduled for shutdown.
		"gpt-5-2025-08-07",
		"gpt-5-mini-2025-08-07",
		"gpt-5-nano-2025-08-07",
		"o3-2025-04-16",
		"gpt-4o-2024-05-13",
	} {
		if _, ok := table.ModelLine("openai", excluded); ok {
			t.Errorf("OpenAI specialized or moving model %q is published through the chat-only contract", excluded)
		}
		if _, ok := table.ModelLine("openai-audio", excluded); ok && excluded != "gpt-5.6" && excluded != "gpt-audio-1.5" {
			t.Errorf("OpenAI session model %q is attributed to the audio adapter", excluded)
		}
	}
}

// TestOpenAIAudioIsAttributedOnlyToTheAudioAdapter pins the reviewed
// transcription and audio chat ids to `openai-audio`, and proves every OpenAI
// row in the checked-in table is one the publisher would actually publish: a
// row the request-family gate drops would be an attribution that silently does
// nothing.
func TestOpenAIAudioIsAttributedOnlyToTheAudioAdapter(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	want := map[string]contract.ModelID{
		"gpt-transcribe":                    "openai/gpt-transcribe",
		"gpt-4o-transcribe":                 "openai/gpt-4o-transcribe",
		"gpt-4o-mini-transcribe-2025-03-20": "openai/gpt-4o-mini-transcribe-2025-03-20",
		"gpt-4o-mini-transcribe-2025-12-15": "openai/gpt-4o-mini-transcribe-2025-12-15",
		"whisper-1":                         "openai/whisper-1",
		"gpt-audio-1.5":                     "openai/gpt-audio-1.5",
	}
	if got := len(table.byProvider["openai-audio"]); got != len(want) {
		t.Fatalf("openai-audio has %d attributions, want exactly the %d reviewed audio models", got, len(want))
	}
	for upstreamModelID, modelLine := range want {
		if got, ok := table.ModelLine("openai-audio", upstreamModelID); !ok || got != modelLine {
			t.Errorf("openai-audio/%s = %q, %t; want %q", upstreamModelID, got, ok, modelLine)
		}
		if _, ok := table.ModelLine("openai", upstreamModelID); ok {
			t.Errorf("audio model %q is attributed to the text chat adapter", upstreamModelID)
		}
	}
	for _, excluded := range []string{
		"gpt-4o-mini-transcribe", "gpt-4o-transcribe-diarize",
		// Retired, or scheduled for shutdown with gpt-audio-1.5 as the replacement.
		"gpt-audio", "gpt-audio-2025-08-28", "gpt-audio-mini", "gpt-audio-mini-2025-10-06", "gpt-audio-mini-2025-12-15",
		"gpt-4o-audio-preview", "gpt-4o-audio-preview-2025-06-03", "gpt-4o-mini-audio-preview",
	} {
		for slug := range openAINamespaces {
			if _, ok := table.ModelLine(slug, excluded); ok {
				t.Errorf("%s/%s is attributed; it is a moving alias, retiring, or has no contract representation", slug, excluded)
			}
		}
	}

	// Every checked-in row, in every namespace the family gate classifies
	// (OpenAI's, OpenRouter's `openai/`, xAI's, Groq's classifiers), is one the
	// publisher would publish under its slug — except the rows the measured
	// configs/inventory.json still names and the gate now withdraws. Those keep
	// their attribution until that snapshot is re-measured (the attribution
	// table's own rule for withdrawn rows), and are listed exactly so the
	// exemption cannot grow one row at a time.
	withdrawn := map[string]bool{
		"groq/meta-llama/llama-prompt-guard-2-22m": true,
		"groq/meta-llama/llama-prompt-guard-2-86m": true,
	}
	exempted := 0
	for slug, models := range table.byProvider {
		target := Provider{Slug: slug, Protocol: providerconfig.Known[slug].Protocol}
		for upstreamModelID := range models {
			key := string(slug) + "/" + upstreamModelID
			switch executes := executable(target, upstreamModelID); {
			case withdrawn[key] && executes:
				t.Errorf("%s is exempted as withdrawn, and the gate would publish it", key)
			case withdrawn[key]:
				exempted++
			case !executes:
				t.Errorf("%s is attributed, and its adapter cannot execute its request family", key)
			}
		}
	}
	if exempted != len(withdrawn) {
		t.Errorf("%d of the %d exempted rows are attributed AND refused by the gate; remove an exemption once its row is gone", exempted, len(withdrawn))
	}
	// OpenRouter's rows of OpenAI's audio chat models answer aloud there.
	for _, upstreamModelID := range []string{"openai/gpt-audio", "openai/gpt-audio-mini"} {
		if _, ok := table.ModelLine("openrouter", upstreamModelID); !ok || !providerconfig.SpeaksAloud("openrouter", providerconfig.ProtocolOpenAICompatible, upstreamModelID) {
			t.Errorf("openrouter/%s is not an attributed deployment that answers aloud", upstreamModelID)
		}
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
		"kilo-code", "llm7", "opencode-zen", "aion-labs", "agnes-ai", "glhf", "ollama",
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
	if got := len(table.byProvider["openrouter"]); got != 383 {
		t.Fatalf("OpenRouter attributions = %d, want the 383 entries whose provenance the checked-in file declares", got)
	}

	supported := map[contract.ProviderSlug]map[string]contract.ModelID{
		"openrouter": {
			"ibm-granite/granite-4.2-8b":          "ibm-granite/granite-4.2-8b",
			"mistralai/devstral-2512":             "mistralai/devstral-2512",
			"qwen/qwen3.8-flash":                  "qwen/qwen3.8-flash",
			"tencent/hy-mt2-7b":                   "tencent/hy-mt2-7b",
			"thinkingmachines/inkling-small:free": "thinkingmachines/inkling-small",
			"thinkingmachines/inkling:free":       "thinkingmachines/inkling",
			"z-ai/glm-5.3-flash":                  "z-ai/glm-5.3-flash",
			// The 2026-09-30 delta.
			"anthropic/claude-opus-5.5":             "anthropic/claude-opus-5.5",
			"anthropic/claude-sonnet-5.5":           "anthropic/claude-sonnet-5.5",
			"google/gemini-3.8-flash":               "google/gemini-3.8-flash",
			"inclusionai/ling-3.0-flash-fin":        "inclusionai/ling-3.0-flash-fin",
			"inclusionai/ling-3.0-flash-sante:free": "inclusionai/ling-3.0-flash-sante",
			"openai/gpt-6-astra":                    "openai/gpt-6-astra",
			"openai/gpt-6.1-sol":                    "openai/gpt-6.1-sol",
			"qwen/qwen3.8-27b:free":                 "qwen/qwen3.8-27b",
			"qwen/qwen3.8-max-0902":                 "qwen/qwen3.8-max-0902",
			"x-ai/grok-4.7":                         "x-ai/grok-4.7",
		},
		"cerebras": {"qwen-3.8-27b": "qwen/qwen3.8-27b"},
		"mistral": {
			"ministral-3b-2512": "mistralai/ministral-3b-2512",
			"codestral-2508":    "mistralai/codestral-2508",
			"zai-glm-5-3":       "z-ai/glm-5.3",
		},
		"xai": {"grok-4.7": "x-ai/grok-4.7"},
		// `Pro/` is a delivery tier, never part of the model line.
		"siliconflow": {
			"Pro/zai-org/GLM-5.1":         "z-ai/glm-5.1",
			"zai-org/GLM-5.3":             "z-ai/glm-5.3",
			"Qwen/Qwen3.8-27B":            "qwen/qwen3.8-27b",
			"Pro/moonshotai/Kimi-K2.6":    "moonshotai/kimi-k2.6",
			"meituan-longcat/LongCat-2.0": "meituan/longcat-2.0",
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

// The 2026-09-30 OpenRouter delta left these unattributed. Each is text output
// under a publisher namespace, so nothing but a reviewed exclusion keeps it out:
// OpenRouter's own descriptions call them multi-model orchestration or
// composite systems, a moving alias, a preview or an undisclosed release.
func TestCheckedInAttributionExcludesTheReviewedOpenRouterSystems(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	for _, excluded := range []string{
		"aion-labs/aion-3.5",
		"aion-labs/aion-3.5-mini",
		"sakana/fugu-max",
		"sakana/fugu-ultra-v2",
		"unbiased/pareto",
		"typesafe/jev-router",
		"openai/gpt-chat-latest",
		"tencent/hy4-preview",
		"stealth/space-bunny-alpha",
	} {
		if _, ok := table.ModelLine("openrouter", excluded); ok {
			t.Errorf("openrouter/%s is attributed; it is a system, moving alias, preview or undisclosed release", excluded)
		}
	}
}

func TestCheckedInAttributionPinsOnlyTheDocumentedNebiusAndNscaleExamples(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}

	want := map[contract.ProviderSlug]map[string]contract.ModelID{
		// Nebius retired both original Meta examples from Serverless; these are
		// the exact ids its 2026 deprecation notices name as replacements.
		"nebius": {
			"deepseek-ai/DeepSeek-V4-Flash-0731": "deepseek/deepseek-v4-flash-0731",
			"MiniMaxAI/MiniMax-M3":               "minimax/minimax-m3",
			"nvidia/nemotron-3-super-120b-a12b":  "nvidia/nemotron-3-super-120b-a12b",
			"nvidia/Nemotron-3_5-Lightning":      "nvidia/nemotron-3.5-lightning",
			"openai/gpt-oss-120b":                "openai/gpt-oss-120b",
			"Qwen/Qwen3.5-397B-A17B":             "qwen/qwen3.5-397b-a17b",
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
		{slug: "nebius", id: "openai/gpt-oss-120b-fast"},
		{slug: "nebius", id: "Qwen/Qwen3.5-397B-A17B-fast"},
		{slug: "nscale", id: "default"},
	} {
		if _, ok := table.ModelLine(candidate.slug, candidate.id); ok {
			t.Errorf("%s/%s attributes a delivery alias rather than fixed weights", candidate.slug, candidate.id)
		}
	}

	// Retired upstream: Nebius removed Llama 3.3 70B from Serverless on
	// 2026-08-31 and no longer lists Llama 3.1 70B; SiliconFlow took GLM-5 and
	// GLM-4.7 offline on 2026-06-11 (GLM-5 requests now reach GLM-5.1, other
	// weights) and Qwen3.5-397B-A17B on 2026-09-11.
	for _, retired := range []struct {
		slug contract.ProviderSlug
		id   string
	}{
		{slug: "nebius", id: "meta-llama/Meta-Llama-3.1-70B-Instruct"},
		{slug: "nebius", id: "meta-llama/Llama-3.3-70B-Instruct"},
		{slug: "siliconflow", id: "Pro/zai-org/GLM-5"},
		{slug: "siliconflow", id: "Pro/zai-org/GLM-4.7"},
		{slug: "siliconflow", id: "Qwen/Qwen3.5-397B-A17B"},
	} {
		if _, ok := table.ModelLine(retired.slug, retired.id); ok {
			t.Errorf("%s/%s is attributed after the provider retired it", retired.slug, retired.id)
		}
	}
}

func TestCheckedInAttributionPinsOnlyDocumentedAlibabaSnapshots(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	want := map[string]contract.ModelID{
		"qwen3.7-max-2026-05-20":   "qwen/qwen3.7-max-2026-05-20",
		"qwen3.7-max-2026-06-08":   "qwen/qwen3.7-max-2026-06-08",
		"qwen3.8-max-0902":         "qwen/qwen3.8-max-0902",
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
	for _, excluded := range []string{"qwen3.7-plus", "qwen3.7-flash", "qwen3.8-max", "qwen3.7-max-preview", "qwen3.7-max", "qwen3.8-flash", "qwen3.7-max-2026-05-17"} {
		if _, ok := table.ModelLine("alibaba", excluded); ok {
			t.Errorf("alibaba/%s attributes a moving or preview id", excluded)
		}
	}
}
