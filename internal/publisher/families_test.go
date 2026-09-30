package publisher

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// TestAnAttributedModelIsPublishedOnlyUnderAnAdapterThatCanExecuteIt is the
// publisher half of the request-family gate. Attribution is deliberately made
// permissive here — every id is attributed under both OpenAI slugs — so that the
// only thing keeping a transcription model off the chat adapter, a chat model
// off the transcription adapter, an audio chat model off the text adapter, a
// text chat model off the audio adapter (which executes chat_completions only to
// answer aloud) and a Realtime model off both is the gate.
func TestAnAttributedModelIsPublishedOnlyUnderAnAdapterThatCanExecuteIt(t *testing.T) {
	ids := []string{"gpt-6-astra", "gpt-transcribe", "whisper-1", "gpt-realtime-2.1", "gpt-realtime-whisper", "gpt-live-transcribe", "gpt-audio-1.5"}
	table := make(map[contract.ProviderSlug]map[string]contract.ModelID)
	for _, slug := range []contract.ProviderSlug{"openai", "openai-audio"} {
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
	built, err := BuildSnapshot([]Discovery{
		{Provider: Provider{Slug: "openai", Protocol: providerconfig.ProtocolOpenAICompatible}, Models: models},
		{Provider: Provider{Slug: "openai-audio", Protocol: providerconfig.ProtocolOpenAIAudio}, Models: models},
	}, attribution, nil, time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	published := make(map[string]bool)
	for _, deployment := range parseSnapshot(t, built.Body).Deployments {
		published[string(deployment.Provider)+"/"+deployment.UpstreamModelID] = true
	}
	want := map[string]bool{"openai/gpt-6-astra": true, "openai-audio/gpt-transcribe": true, "openai-audio/whisper-1": true, "openai-audio/gpt-audio-1.5": true}
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
	if len(built.Inexecutable) != 2*len(ids)-len(want) {
		t.Errorf("inexecutable = %v; every dropped pair must be named for the operator", built.Inexecutable)
	}
}

func TestOpenAIRequestFamilies(t *testing.T) {
	type family struct {
		format contract.APIFormat
		spoken bool
	}
	cases := map[string]family{
		"gpt-6-astra":            {contract.APIFormatChatCompletions, false},
		"gpt-5.6-sol":            {contract.APIFormatChatCompletions, false},
		"gpt-audio-1.5":          {contract.APIFormatChatCompletions, true},
		"gpt-audio-2025-08-28":   {contract.APIFormatChatCompletions, true},
		"gpt-audio-mini":         {contract.APIFormatChatCompletions, true},
		"gpt-4o-audio-preview":   {contract.APIFormatChatCompletions, true},
		"gpt-transcribe":         {contract.APIFormatAudioTranscriptions, false},
		"gpt-4o-transcribe":      {contract.APIFormatAudioTranscriptions, false},
		"gpt-4o-mini-transcribe": {contract.APIFormatAudioTranscriptions, false},
		"whisper-1":              {contract.APIFormatAudioTranscriptions, false},
		"gpt-4o-mini-tts":        {contract.APIFormatAudioSpeech, false},
		"tts-1-hd":               {contract.APIFormatAudioSpeech, false},
		"gpt-image-1":            {contract.APIFormatImagesGenerations, false},
		"text-embedding-3-large": {contract.APIFormatEmbeddings, false},
	}
	for id, want := range cases {
		if format, spoken, ok := openAIRequestFamily(id); !ok || format != want.format || spoken != want.spoken {
			t.Errorf("%s = %q spoken=%t, %t; want %q spoken=%t", id, format, spoken, ok, want.format, want.spoken)
		}
	}
	for _, session := range []string{
		"gpt-realtime-2.1", "gpt-realtime-2.1-mini", "gpt-realtime-2", "gpt-realtime-translate",
		"gpt-live-transcribe", "gpt-realtime-whisper", "gpt-realtime-1.5", "gpt-live-1",
		"gpt-4o-realtime-preview", "gpt-4o-mini-realtime-preview", "omni-moderation-latest",
	} {
		if format, _, ok := openAIRequestFamily(session); ok {
			t.Errorf("%s classified as %q; no contract family can execute it", session, format)
		}
	}
}

// TestSpokenChatIsExecutableOnlyByTheAudioAdapter is the exclusive-or the
// request-family gate adds for chat_completions: both OpenAI slugs execute the
// family, and exactly one of them can speak.
func TestSpokenChatIsExecutableOnlyByTheAudioAdapter(t *testing.T) {
	text := Provider{Slug: "openai", Protocol: providerconfig.ProtocolOpenAICompatible}
	audio := Provider{Slug: "openai-audio", Protocol: providerconfig.ProtocolOpenAIAudio}
	cases := []struct {
		target Provider
		id     string
		want   bool
	}{
		{audio, "gpt-audio-1.5", true},
		{text, "gpt-audio-1.5", false},
		{text, "gpt-6-astra", true},
		{audio, "gpt-6-astra", false},
		{audio, "gpt-transcribe", true},
		{text, "gpt-transcribe", false},
		// The protocol falls back to the built-in table when a discovery
		// target names none.
		{Provider{Slug: "openai-audio"}, "gpt-audio-1.5", true},
		{Provider{Slug: "openai"}, "gpt-audio-1.5", false},
	}
	for _, c := range cases {
		if got := executable(c.target, c.id); got != c.want {
			t.Errorf("%s/%s executable = %t, want %t", c.target.Slug, c.id, got, c.want)
		}
	}
}
