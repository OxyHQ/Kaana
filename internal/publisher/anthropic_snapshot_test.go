package publisher

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// TestADirectAnthropicDeploymentJoinsTheOpenRouterLine is what attributing to
// the SAME line buys: with the checked-in table, a direct Anthropic deployment
// and OpenRouter's row of the same weights are one revision-pinned reference
// with two endpoints — the failover set — and the direct one inherits the
// line's carried-forward observation date instead of minting a second
// revision, which the reader would refuse as two `current` revisions.
func TestADirectAnthropicDeploymentJoinsTheOpenRouterLine(t *testing.T) {
	attribution, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	discoveries := []Discovery{
		{
			Provider: Provider{Slug: "openrouter", Protocol: providerconfig.ProtocolOpenAICompatible, Discovery: providerconfig.DiscoveryOpenAIModels},
			Models:   []DiscoveredModel{{UpstreamModelID: "anthropic/claude-opus-5.5"}},
		},
		{
			Provider: Provider{Slug: "anthropic", Protocol: providerconfig.ProtocolAnthropicMessages, Discovery: providerconfig.DiscoveryAnthropicModels},
			Models: []DiscoveredModel{
				{UpstreamModelID: "claude-opus-5-5"},
				{UpstreamModelID: "claude-haiku-5-5"},
				// An alias Anthropic resolves to a dated id: dropped and named.
				{UpstreamModelID: "claude-haiku-4-5"},
			},
		},
	}
	previous := Observations{"anthropic/claude-opus-5.5": "2026-09-30"}
	built, err := BuildSnapshot(discoveries, attribution, previous, time.Date(2026, time.October, 12, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	got := map[contract.DeploymentID]contract.ModelReference{}
	for _, deployment := range parseSnapshot(t, built.Body).Deployments {
		got[deployment.DeploymentID] = deployment.ModelReference
	}
	want := map[contract.DeploymentID]contract.ModelReference{
		"dep_openrouter_anthropic_claude_opus_5_5_observed_2026_09_30": "anthropic/claude-opus-5.5@observed-2026-09-30",
		"dep_anthropic_claude_opus_5_5_observed_2026_09_30":            "anthropic/claude-opus-5.5@observed-2026-09-30",
		"dep_anthropic_claude_haiku_5_5_observed_2026_10_12":           "anthropic/claude-haiku-5.5@observed-2026-10-12",
	}
	if len(got) != len(want) {
		t.Fatalf("deployments = %v, want %v", got, want)
	}
	for id, reference := range want {
		if got[id] != reference {
			t.Errorf("%s = %q, want %q", id, got[id], reference)
		}
	}
	if len(built.Unattributed) != 1 || built.Unattributed[0] != "anthropic/claude-haiku-4-5" {
		t.Errorf("unattributed = %v, want the pre-4.6 alias named", built.Unattributed)
	}
}
