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
// off the transcription adapter and a Realtime model off both is the gate.
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
	want := map[string]bool{"openai/gpt-6-astra": true, "openai-audio/gpt-transcribe": true, "openai-audio/whisper-1": true}
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
	cases := map[string]contract.APIFormat{
		"gpt-6-astra":            contract.APIFormatChatCompletions,
		"gpt-5.6-sol":            contract.APIFormatChatCompletions,
		"gpt-transcribe":         contract.APIFormatAudioTranscriptions,
		"gpt-4o-transcribe":      contract.APIFormatAudioTranscriptions,
		"gpt-4o-mini-transcribe": contract.APIFormatAudioTranscriptions,
		"whisper-1":              contract.APIFormatAudioTranscriptions,
		"gpt-4o-mini-tts":        contract.APIFormatAudioSpeech,
		"tts-1-hd":               contract.APIFormatAudioSpeech,
		"gpt-image-1":            contract.APIFormatImagesGenerations,
		"text-embedding-3-large": contract.APIFormatEmbeddings,
	}
	for id, want := range cases {
		if got, ok := openAIRequestFamily(id); !ok || got != want {
			t.Errorf("%s = %q, %t; want %q", id, got, ok, want)
		}
	}
	for _, session := range []string{
		"gpt-realtime-2.1", "gpt-realtime-2.1-mini", "gpt-realtime-2", "gpt-realtime-translate",
		"gpt-live-transcribe", "gpt-realtime-whisper", "gpt-realtime-1.5", "gpt-live-1",
		"gpt-audio-1.5", "gpt-4o-audio-preview", "omni-moderation-latest",
	} {
		if family, ok := openAIRequestFamily(session); ok {
			t.Errorf("%s classified as %q; no contract family can execute it", session, family)
		}
	}
}
