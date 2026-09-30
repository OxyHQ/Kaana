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
