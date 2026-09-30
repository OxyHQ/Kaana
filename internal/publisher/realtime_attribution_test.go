package publisher

import (
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// TestOpenAIRealtimeIsAttributedOnlyToTheRealtimeAdapter pins the reviewed
// Realtime conversation ids to `openai-realtime` and nowhere else, keeps every
// session kind the adapter does not open out of the table, and proves each row
// is one the publisher would actually publish there and refuse anywhere else.
func TestOpenAIRealtimeIsAttributedOnlyToTheRealtimeAdapter(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	want := map[string]contract.ModelID{
		"gpt-realtime-2.1":      "openai/gpt-realtime-2.1",
		"gpt-realtime-2.1-mini": "openai/gpt-realtime-2.1-mini",
		"gpt-realtime-2":        "openai/gpt-realtime-2",
	}
	if got := len(table.byProvider["openai-realtime"]); got != len(want) {
		t.Fatalf("openai-realtime has %d attributions, want exactly the %d reviewed conversation models", got, len(want))
	}
	realtime := Provider{Slug: "openai-realtime", Protocol: providerconfig.ProtocolOpenAIRealtime}
	for upstreamModelID, modelLine := range want {
		if got, ok := table.ModelLine("openai-realtime", upstreamModelID); !ok || got != modelLine {
			t.Errorf("openai-realtime/%s = %q, %t; want %q", upstreamModelID, got, ok, modelLine)
		}
		if !executable(realtime, upstreamModelID) {
			t.Errorf("openai-realtime/%s is attributed and the realtime adapter would not publish it", upstreamModelID)
		}
		for _, other := range []contract.ProviderSlug{"openai", "openai-audio"} {
			if _, ok := table.ModelLine(other, upstreamModelID); ok {
				t.Errorf("realtime model %q is attributed to %s", upstreamModelID, other)
			}
			if executable(Provider{Slug: other, Protocol: providerconfig.Known[other].Protocol}, upstreamModelID) {
				t.Errorf("realtime model %q would be published under the request adapter %s", upstreamModelID, other)
			}
		}
	}
	for _, excluded := range []string{"gpt-realtime-translate", "gpt-live-transcribe", "gpt-realtime-whisper", "gpt-live-1", "gpt-realtime", "gpt-realtime-mini"} {
		for slug := range openAINamespaces {
			if _, ok := table.ModelLine(slug, excluded); ok {
				t.Errorf("%s/%s is attributed: a session kind no adapter opens, another protocol, or a moving alias", slug, excluded)
			}
		}
	}
}

// TestXAIVoiceIsAttributedOnlyToTheRealtimeAdapter pins the pinned xAI voice
// model to `xai-realtime` and nowhere else, keeps the moving alias out, and
// proves the row is one the publisher would publish there and refuse under the
// `xai` request adapter.
func TestXAIVoiceIsAttributedOnlyToTheRealtimeAdapter(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	if got := len(table.byProvider["xai-realtime"]); got != 1 {
		t.Fatalf("xai-realtime has %d attributions, want exactly the pinned voice model", got)
	}
	if got, ok := table.ModelLine("xai-realtime", "grok-voice-think-fast-2.0"); !ok || got != "x-ai/grok-voice-think-fast-2.0" {
		t.Errorf("xai-realtime/grok-voice-think-fast-2.0 = %q, %t", got, ok)
	}
	if !executable(Provider{Slug: "xai-realtime", Protocol: providerconfig.ProtocolXAIRealtime}, "grok-voice-think-fast-2.0") {
		t.Error("the voice model is attributed and the xAI realtime adapter would not publish it")
	}
	if executable(Provider{Slug: "xai", Protocol: providerconfig.ProtocolOpenAICompatible}, "grok-voice-think-fast-2.0") {
		t.Error("the voice model would be published under the xai request adapter")
	}
	for _, slug := range []contract.ProviderSlug{"xai", "xai-realtime"} {
		if _, ok := table.ModelLine(slug, "grok-voice-latest"); ok {
			t.Errorf("%s/grok-voice-latest is attributed; it is a moving alias", slug)
		}
	}
	if _, ok := table.ModelLine("xai", "grok-voice-think-fast-2.0"); ok {
		t.Error("the voice model is attributed to the xai request adapter")
	}
}
