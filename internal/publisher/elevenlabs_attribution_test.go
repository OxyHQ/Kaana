package publisher

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider/elevenlabs"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// TestCheckedInElevenLabsAttributionHoldsOnlyCurrentSpeechModels pins the
// reviewed allow-list (docs/elevenlabs.md): three GA text-to-speech ids, each
// executable by the speech adapter and each with the adapter's reviewed
// character limit, and none of the deprecated, dialogue-only, realtime,
// English-only-duplicate or other-family ids.
func TestCheckedInElevenLabsAttributionHoldsOnlyCurrentSpeechModels(t *testing.T) {
	table, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	want := map[string]contract.ModelID{
		"eleven_multilingual_v2": "elevenlabs/eleven-multilingual-v2",
		"eleven_flash_v2_5":      "elevenlabs/eleven-flash-v2.5",
		"eleven_v3":              "elevenlabs/eleven-v3",
	}
	if got := len(table.byProvider["elevenlabs"]); got != len(want) {
		t.Fatalf("ElevenLabs has %d attributions, want exactly the %d reviewed speech models", got, len(want))
	}
	target := Provider{Slug: "elevenlabs", Protocol: providerconfig.ProtocolElevenLabsSpeech}
	for upstreamModelID, modelLine := range want {
		if got, ok := table.ModelLine("elevenlabs", upstreamModelID); !ok || got != modelLine {
			t.Errorf("elevenlabs/%s = %q, %t; want %q", upstreamModelID, got, ok, modelLine)
		}
		if !executable(target, upstreamModelID) {
			t.Errorf("elevenlabs/%s is attributed but the speech adapter cannot execute it", upstreamModelID)
		}
		if _, reviewed := elevenlabs.MaxCharacters[upstreamModelID]; !reviewed {
			t.Errorf("elevenlabs/%s is attributed but the adapter has no reviewed character limit, so every request to it would be refused", upstreamModelID)
		}
	}
	for id := range elevenlabs.MaxCharacters {
		if _, ok := want[id]; !ok {
			t.Errorf("the adapter reviews a character limit for %s, which is not attributed", id)
		}
	}
	for _, excluded := range []string{
		"eleven_turbo_v2_5", "eleven_turbo_v2", // deprecated
		"eleven_flash_v2",                                          // English-only subset of eleven_flash_v2_5 at the same price
		"eleven_v4", "eleven_v4_turbo", "eleven_v3_conversational", // dialogue API / realtime
		"eleven_multilingual_sts_v2", "eleven_english_sts_v2", "eleven_ttv_v3", "eleven_multilingual_ttv_v2",
		"eleven_text_to_sound_v2", "music_v2", "scribe_v2", "scribe_v2_realtime", "eleven_monolingual_v1", "eleven_multilingual_v1",
	} {
		if _, ok := table.ModelLine("elevenlabs", excluded); ok {
			t.Errorf("elevenlabs/%s is attributed; it is deprecated, served by another API or another request family", excluded)
		}
	}
}

// TestElevenLabsDeploymentsPublishAsSpeech: the checked-in table publishes the
// two discovered GA ids and drops, by name, the deprecated Turbo row the
// account list still returns.
func TestElevenLabsDeploymentsPublishAsSpeech(t *testing.T) {
	attribution, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	discoveries := []Discovery{{
		Provider: Provider{Slug: "elevenlabs", Protocol: providerconfig.ProtocolElevenLabsSpeech, Discovery: providerconfig.DiscoveryElevenLabsModels},
		Models: []DiscoveredModel{
			{UpstreamModelID: "eleven_flash_v2_5"},
			{UpstreamModelID: "eleven_multilingual_v2"},
			{UpstreamModelID: "eleven_turbo_v2_5"},
		},
	}}
	built, err := BuildSnapshot(discoveries, attribution, Observations{}, time.Date(2026, time.October, 12, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	got := map[contract.DeploymentID]contract.ModelReference{}
	for _, deployment := range parseSnapshot(t, built.Body).Deployments {
		if deployment.Provider != "elevenlabs" || deployment.UpstreamModelID == "" {
			t.Errorf("deployment = %+v", deployment)
		}
		got[deployment.DeploymentID] = deployment.ModelReference
	}
	want := map[contract.DeploymentID]contract.ModelReference{
		"dep_elevenlabs_eleven_flash_v2_5_observed_2026_10_12":      "elevenlabs/eleven-flash-v2.5@observed-2026-10-12",
		"dep_elevenlabs_eleven_multilingual_v2_observed_2026_10_12": "elevenlabs/eleven-multilingual-v2@observed-2026-10-12",
	}
	if len(got) != len(want) {
		t.Fatalf("deployments = %v, want %v", got, want)
	}
	for id, reference := range want {
		if got[id] != reference {
			t.Errorf("%s = %q, want %q", id, got[id], reference)
		}
	}
	if len(built.Unattributed) != 1 || built.Unattributed[0] != "elevenlabs/eleven_turbo_v2_5" {
		t.Errorf("unattributed = %v, want the deprecated Turbo row named", built.Unattributed)
	}
}
