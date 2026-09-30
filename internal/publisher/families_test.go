package publisher

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// TestAnAttributedModelIsPublishedOnlyUnderAnAdapterThatCanExecuteIt is the
// publisher half of the request-family gate. Attribution is deliberately made
// permissive here — every id is attributed under all three OpenAI slugs — so
// that the only thing keeping a transcription model off the chat adapter, a
// chat model off the transcription adapter, an audio chat model off the text
// adapter, a text chat model off the audio adapter (which executes
// chat_completions only to answer aloud), a Realtime conversation model off
// both request adapters, and a session kind the realtime adapter does not open
// off it too, is the gate.
func TestAnAttributedModelIsPublishedOnlyUnderAnAdapterThatCanExecuteIt(t *testing.T) {
	ids := []string{"gpt-6-astra", "gpt-transcribe", "whisper-1", "gpt-realtime-2.1", "gpt-realtime-whisper", "gpt-live-transcribe", "gpt-realtime-translate", "gpt-live-1", "gpt-audio-1.5"}
	slugs := map[contract.ProviderSlug]string{
		"openai":          providerconfig.ProtocolOpenAICompatible,
		"openai-audio":    providerconfig.ProtocolOpenAIAudio,
		"openai-realtime": providerconfig.ProtocolOpenAIRealtime,
	}
	table := make(map[contract.ProviderSlug]map[string]contract.ModelID)
	for slug := range slugs {
		table[slug] = make(map[string]contract.ModelID)
		for _, id := range ids {
			table[slug][id] = contract.ModelID("openai/" + id)
		}
	}
	attribution := &Attribution{byProvider: table}

	models := make([]DiscoveredModel, 0, len(ids))
	for _, id := range ids {
		models = append(models, DiscoveredModel{UpstreamModelID: id})
	}
	discoveries := make([]Discovery, 0, len(slugs))
	for slug, protocol := range slugs {
		discoveries = append(discoveries, Discovery{Provider: Provider{Slug: slug, Protocol: protocol}, Models: models})
	}
	built, err := BuildSnapshot(discoveries, attribution, nil, time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	published := make(map[string]bool)
	for _, deployment := range parseSnapshot(t, built.Body).Deployments {
		published[string(deployment.Provider)+"/"+deployment.UpstreamModelID] = true
	}
	want := map[string]bool{
		"openai/gpt-6-astra": true, "openai-audio/gpt-transcribe": true, "openai-audio/whisper-1": true,
		"openai-audio/gpt-audio-1.5": true, "openai-realtime/gpt-realtime-2.1": true,
	}
	for key := range want {
		if !published[key] {
			t.Errorf("%s was not published under the adapter that executes it", key)
		}
	}
	for key := range published {
		if !want[key] {
			t.Errorf("%s was published under an adapter that cannot execute it", key)
		}
	}
	if len(built.Inexecutable) != len(slugs)*len(ids)-len(want) {
		t.Errorf("inexecutable = %v; every dropped pair must be named for the operator", built.Inexecutable)
	}
}

// openAINamespaces are the slugs that list OpenAI's own model ids.
var openAINamespaces = map[contract.ProviderSlug]bool{"openai": true, "openai-audio": true, "openai-realtime": true}

// TestSpokenChatIsExecutableOnlyWhereTheDeploymentSpeaks is the gate the
// request family adds for chat_completions, decided per slug and model: both
// OpenAI request slugs execute the family and exactly one of them can speak;
// OpenRouter's adapter writes AND speaks, so its `openai/gpt-audio*` rows
// publish there while a session model under it does not. The realtime slugs
// execute no request, and xAI's voice model publishes only under its session
// slug.
func TestSpokenChatIsExecutableOnlyWhereTheDeploymentSpeaks(t *testing.T) {
	text := Provider{Slug: "openai", Protocol: providerconfig.ProtocolOpenAICompatible}
	audio := Provider{Slug: "openai-audio", Protocol: providerconfig.ProtocolOpenAIAudio}
	realtime := Provider{Slug: "openai-realtime", Protocol: providerconfig.ProtocolOpenAIRealtime}
	gateway := Provider{Slug: "openrouter", Protocol: providerconfig.ProtocolOpenAICompatible}
	xai := Provider{Slug: "xai", Protocol: providerconfig.ProtocolOpenAICompatible}
	xaiRealtime := Provider{Slug: "xai-realtime", Protocol: providerconfig.ProtocolXAIRealtime}
	cases := []struct {
		target Provider
		id     string
		want   bool
	}{
		{audio, "gpt-audio-1.5", true},
		{text, "gpt-audio-1.5", false},
		{realtime, "gpt-audio-1.5", false},
		{text, "gpt-6-astra", true},
		{audio, "gpt-6-astra", false},
		{realtime, "gpt-6-astra", false},
		{audio, "gpt-transcribe", true},
		{text, "gpt-transcribe", false},
		{realtime, "gpt-realtime-2.1", true},
		{realtime, "gpt-realtime-translate", false},
		{realtime, "gpt-realtime-whisper", false},
		// The protocol falls back to the built-in table when a discovery
		// target names none.
		{Provider{Slug: "openai-audio"}, "gpt-audio-1.5", true},
		{Provider{Slug: "openai"}, "gpt-audio-1.5", false},
		{Provider{Slug: "openai-realtime"}, "gpt-realtime-2.1", true},
		{gateway, "openai/gpt-audio", true},
		{gateway, "openai/gpt-audio-mini", true},
		{gateway, "openai/gpt-6-astra", true},
		{gateway, "openai/gpt-realtime-2", false},
		{gateway, "openai/gpt-image-2", false},
		{gateway, "google/gemini-3.8-flash", true}, // unclassified: attribution decides
		{xaiRealtime, "grok-voice-think-fast-2.0", true},
		{xai, "grok-voice-think-fast-2.0", false},
		{xaiRealtime, "grok-4.7", false},
		{xaiRealtime, "tts", false},
		{xai, "grok-4.7", true},
		{xai, "tts", true},
		{Provider{Slug: "xai-realtime"}, "grok-voice-think-fast-2.0", true},
	}
	for _, c := range cases {
		if got := executable(c.target, c.id); got != c.want {
			t.Errorf("%s/%s executable = %t, want %t", c.target.Slug, c.id, got, c.want)
		}
	}
}
