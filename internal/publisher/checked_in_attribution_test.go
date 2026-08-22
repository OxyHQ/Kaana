package publisher

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/OxyHQ/Relay/internal/contract"
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
