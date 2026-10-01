package providerconfig

import (
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestOpenAIFamilies(t *testing.T) {
	requests := map[string]ModelFamily{
		"gpt-6-astra":            {Format: contract.APIFormatChatCompletions},
		"gpt-5.6-sol":            {Format: contract.APIFormatChatCompletions},
		"gpt-audio-1.5":          {Format: contract.APIFormatChatCompletions, Spoken: true},
		"gpt-audio-2025-08-28":   {Format: contract.APIFormatChatCompletions, Spoken: true},
		"gpt-audio-mini":         {Format: contract.APIFormatChatCompletions, Spoken: true},
		"gpt-4o-audio-preview":   {Format: contract.APIFormatChatCompletions, Spoken: true},
		"gpt-transcribe":         {Format: contract.APIFormatAudioTranscriptions},
		"gpt-4o-transcribe":      {Format: contract.APIFormatAudioTranscriptions},
		"gpt-4o-mini-transcribe": {Format: contract.APIFormatAudioTranscriptions},
		"whisper-1":              {Format: contract.APIFormatAudioTranscriptions},
		"gpt-4o-mini-tts":        {Format: contract.APIFormatAudioSpeech},
		"tts-1-hd":               {Format: contract.APIFormatAudioSpeech},
		"gpt-image-1":            {Format: contract.APIFormatImagesGenerations},
		"text-embedding-3-large": {Format: contract.APIFormatEmbeddings},
	}
	sessions := map[string]contract.RealtimeSessionKind{
		"gpt-realtime-2.1":             contract.RealtimeConversation,
		"gpt-realtime-2.1-mini":        contract.RealtimeConversation,
		"gpt-realtime-2":               contract.RealtimeConversation,
		"gpt-realtime-1.5":             contract.RealtimeConversation,
		"gpt-realtime-2025-08-28":      contract.RealtimeConversation,
		"gpt-4o-realtime-preview":      contract.RealtimeConversation,
		"gpt-4o-mini-realtime-preview": contract.RealtimeConversation,
		"gpt-realtime-translate":       contract.RealtimeTranslation,
		"gpt-live-transcribe":          contract.RealtimeTranscription,
		"gpt-realtime-whisper":         contract.RealtimeTranscription,
	}
	for id, session := range sessions {
		requests[id] = ModelFamily{Session: session}
	}
	for id, want := range requests {
		if got, ok := openAIFamily(id); !ok || got != want {
			t.Errorf("%s = %+v, %t; want %+v", id, got, ok, want)
		}
	}
	for _, inexpressible := range []string{"gpt-live-1", "omni-moderation-latest"} {
		if got, ok := openAIFamily(inexpressible); ok {
			t.Errorf("%s classified as %+v; nothing the contract names can execute it", inexpressible, got)
		}
	}
	// OpenRouter's `openai/` rows are the same models under the gateway's
	// namespace; its other rows say nothing about their family.
	if got, classified, _ := ClassifyModel("openrouter", "openai/gpt-audio-mini"); !classified || !got.Spoken {
		t.Errorf("openrouter/openai/gpt-audio-mini = %+v, %t", got, classified)
	}
	if _, classified, _ := ClassifyModel("openrouter", "google/gemini-3.8-flash"); classified {
		t.Error("an OpenRouter row outside the openai/ namespace was classified")
	}
}

// TestSpeakingIsDecidedPerDeployment is the per-slug-and-model capability the
// executor and the publisher both read. An adapter that can speak serving a
// model that cannot, and a model that speaks behind an adapter that cannot,
// are both refused; each positive row is the control that keeps the refusals
// from passing because nothing speaks at all.
func TestSpeakingIsDecidedPerDeployment(t *testing.T) {
	cases := []struct {
		slug     contract.ProviderSlug
		protocol string
		model    string
		want     bool
	}{
		{"openai-audio", ProtocolOpenAIAudio, "gpt-audio-1.5", true},
		{"openrouter", ProtocolOpenAICompatible, "openai/gpt-audio", true},
		{"openrouter", ProtocolOpenAICompatible, "openai/gpt-audio-mini", true},
		// A speaking adapter, a text model.
		{"openrouter", ProtocolOpenAICompatible, "openai/gpt-6-astra", false},
		{"openrouter", ProtocolOpenAICompatible, "anthropic/claude-sonnet-5.5", false},
		{"openai-audio", ProtocolOpenAIAudio, "gpt-transcribe", false},
		// A speaking model, an adapter that cannot.
		{"openai", ProtocolOpenAICompatible, "gpt-audio-1.5", false},
		{"groq", ProtocolOpenAICompatible, "openai/gpt-audio", false},
		{"xai", ProtocolOpenAICompatible, "grok-voice-think-fast-2.0", false},
		{"anthropic", ProtocolAnthropicMessages, "gpt-audio", false},
	}
	for _, c := range cases {
		if got := SpeaksAloud(c.slug, c.protocol, c.model); got != c.want {
			t.Errorf("%s (%s) %s speaks aloud: %t, want %t", c.slug, c.protocol, c.model, got, c.want)
		}
	}
}

func TestChatOutputsPerSlug(t *testing.T) {
	cases := map[contract.ProviderSlug]struct {
		protocol string
		want     ChatOutput
	}{
		"openai-audio":    {ProtocolOpenAIAudio, ChatOutput{Spoken: true}},
		"openrouter":      {ProtocolOpenAICompatible, ChatOutput{Text: true, Spoken: true}},
		"openai":          {ProtocolOpenAICompatible, ChatOutput{Text: true}},
		"xai":             {ProtocolOpenAICompatible, ChatOutput{Text: true}},
		"anthropic":       {ProtocolAnthropicMessages, ChatOutput{Text: true}},
		"deepgram":        {ProtocolDeepgramVoice, ChatOutput{}},
		"openai-realtime": {ProtocolOpenAIRealtime, ChatOutput{}},
		"xai-realtime":    {ProtocolXAIRealtime, ChatOutput{}},
	}
	for slug, c := range cases {
		if got := ChatOutputs(slug, c.protocol); got != c.want {
			t.Errorf("%s chat outputs = %+v, want %+v", slug, got, c.want)
		}
	}
}

func TestXAIFamilies(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"xai", "xai-realtime"} {
		for id, want := range map[string]ModelFamily{
			"grok-voice-think-fast-2.0": {Session: contract.RealtimeConversation},
			"grok-voice-latest":         {Session: contract.RealtimeConversation},
			"tts":                       {Format: contract.APIFormatAudioSpeech},
			"grok-4.7":                  {Format: contract.APIFormatChatCompletions},
		} {
			got, classified, expressible := ClassifyModel(slug, id)
			if !classified || !expressible || got != want {
				t.Errorf("%s/%s = %+v (%t, %t), want %+v", slug, id, got, classified, expressible, want)
			}
		}
	}
	if _, classified, _ := ClassifyModel("groq", "grok-voice-think-fast-2.0"); classified {
		t.Error("an id is classified outside the namespace that names it")
	}
}

func TestJevFamiliesRemainUnpublishable(t *testing.T) {
	for _, item := range []struct {
		slug  contract.ProviderSlug
		model string
	}{
		{"typesafe", "jev-1.13.0"}, {"typesafe", "jev-latest"},
		{"openrouter", "typesafe/jev-1.13"}, {"openrouter", "typesafe/jev-1.13-20260917"},
		{"openrouter", "~typesafe/jev-latest"}, {"openrouter", "typesafe/jev-router"},
	} {
		family, classified, expressible := ClassifyModel(item.slug, item.model)
		if !classified || expressible || family.Format != contract.APIFormatDecisions {
			t.Fatalf("Jev row can publish: %s/%s", item.slug, item.model)
		}
	}
	family, classified, expressible := ClassifyModel("openrouter", "openai/gpt-6-sol")
	if !classified || !expressible || family.Format != contract.APIFormatChatCompletions {
		t.Fatal("ordinary chat positive control blocked")
	}
}
