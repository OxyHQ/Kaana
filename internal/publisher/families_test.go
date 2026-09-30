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

func TestOpenAIFamilies(t *testing.T) {
	requests := map[string]family{
		"gpt-6-astra":            {format: contract.APIFormatChatCompletions},
		"gpt-5.6-sol":            {format: contract.APIFormatChatCompletions},
		"gpt-audio-1.5":          {format: contract.APIFormatChatCompletions, spoken: true},
		"gpt-audio-2025-08-28":   {format: contract.APIFormatChatCompletions, spoken: true},
		"gpt-audio-mini":         {format: contract.APIFormatChatCompletions, spoken: true},
		"gpt-4o-audio-preview":   {format: contract.APIFormatChatCompletions, spoken: true},
		"gpt-transcribe":         {format: contract.APIFormatAudioTranscriptions},
		"gpt-4o-transcribe":      {format: contract.APIFormatAudioTranscriptions},
		"gpt-4o-mini-transcribe": {format: contract.APIFormatAudioTranscriptions},
		"whisper-1":              {format: contract.APIFormatAudioTranscriptions},
		"gpt-4o-mini-tts":        {format: contract.APIFormatAudioSpeech},
		"tts-1-hd":               {format: contract.APIFormatAudioSpeech},
		"gpt-image-1":            {format: contract.APIFormatImagesGenerations},
		"text-embedding-3-large": {format: contract.APIFormatEmbeddings},
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
		requests[id] = family{session: session}
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
}

// TestSpokenChatIsExecutableOnlyByTheAudioAdapter is the exclusive-or the
// request-family gate adds for chat_completions: both OpenAI request slugs
// execute the family, and exactly one of them can speak. The realtime slug
// executes neither.
func TestSpokenChatIsExecutableOnlyByTheAudioAdapter(t *testing.T) {
	text := Provider{Slug: "openai", Protocol: providerconfig.ProtocolOpenAICompatible}
	audio := Provider{Slug: "openai-audio", Protocol: providerconfig.ProtocolOpenAIAudio}
	realtime := Provider{Slug: "openai-realtime", Protocol: providerconfig.ProtocolOpenAIRealtime}
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
	}
	for _, c := range cases {
		if got := executable(c.target, c.id); got != c.want {
			t.Errorf("%s/%s executable = %t, want %t", c.target.Slug, c.id, got, c.want)
		}
	}
}
