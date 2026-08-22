package inventory

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
)

// The checked-in snapshot is the one artefact in this repository that no other
// test reads: `go build` and `go test` are both green against a file that would
// stop the process at boot. Two failures reached that state while this file was
// being written — a model line left with two current revisions, and a reference
// carrying a character the parser rejects — and neither was visible anywhere
// except by loading it.
//
// So this loads it exactly as the server does, and then asserts the three
// exclusion rules the snapshot's own `$comment` claims to follow. Those rules
// are what a careless regeneration silently drops: an upstream id that picks a
// different model per request is well-formed JSON, loads without complaint, and
// only misbehaves in front of a customer.
const checkedInPath = "../../configs/inventory.json"

func loadCheckedIn(t *testing.T) *Inventory {
	t.Helper()
	loaded, err := Load(checkedInPath, time.Hour)
	if err != nil {
		t.Fatalf("the checked-in snapshot does not load: %v", err)
	}
	return loaded
}

func TestCheckedInSnapshotLoads(t *testing.T) {
	loaded := loadCheckedIn(t)

	// The vacuity floor. An emptied or truncated snapshot loads fine and would
	// satisfy every assertion below over nothing.
	if got := len(loaded.Deployments()); got < 100 {
		t.Fatalf("declared deployments = %d, want at least 100 — snapshot looks emptied", got)
	}
	if len(loaded.Providers()) == 0 {
		t.Fatal("snapshot routes to no provider")
	}

	// Every provider it routes to must have an adapter at boot, so a slug that
	// nobody configures is a process that will not start.
	t.Logf("providers: %v, deployments: %d", loaded.Providers(), len(loaded.Deployments()))
}

func TestCheckedInSnapshotDeclaresNothingThatSubstitutes(t *testing.T) {
	raw, err := os.ReadFile(checkedInPath)
	if err != nil {
		t.Fatalf("read: %v", err)
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
		t.Fatalf("parse: %v", err)
	}
	if len(file.Deployments) < 100 {
		t.Fatalf("parsed %d deployments, want at least 100 — the shape this test reads may have changed", len(file.Deployments))
	}

	// The rule these three enforce is one rule: a reference names ONE model, and
	// the upstream id it maps to must name that same model tomorrow.
	for _, d := range file.Deployments {
		switch {
		case strings.HasPrefix(d.UpstreamModelID, "openrouter/"):
			t.Errorf("%s declares %q: a router that picks a different model per request", d.DeploymentID, d.UpstreamModelID)
		case strings.HasSuffix(strings.SplitN(d.UpstreamModelID, ":", 2)[0], "-latest"):
			t.Errorf("%s declares %q: a moving alias whose weights change without the id changing", d.DeploymentID, d.UpstreamModelID)
		case strings.HasSuffix(d.UpstreamModelID, ":batch"), strings.HasSuffix(d.UpstreamModelID, ":thinking"):
			t.Errorf("%s declares %q: a delivery mode, not a set of weights — a reference has no slot for it", d.DeploymentID, d.UpstreamModelID)
		}
		// The publisher slot is who released the weights. A reference naming the
		// serving provider makes a model's identity change the day a second
		// provider serves it.
		//
		// The two exemptions are slugs that would ALSO be legitimate publishers,
		// so this check is inert for them by construction. Neither is a provider
		// today; the day one is, it is exempt and something else has to catch it.
		if strings.HasPrefix(d.ModelReference, d.Provider+"/") && d.Provider != "openai" && d.Provider != "google" {
			t.Errorf("%s: reference %q names the serving provider as publisher", d.DeploymentID, d.ModelReference)
		}
	}
}

func TestCheckedInSnapshotRefusesWhatItDoesNotDeclare(t *testing.T) {
	loaded := loadCheckedIn(t)
	at := time.Now()

	// Negative controls. Without these, a Resolve that answered everything would
	// pass any positive assertion made above it.
	for _, absent := range []string{
		"openrouter/auto@observed-2026-08-23",
		"~z-ai/glm-latest@observed-2026-08-23",
		"definitely-not-a-publisher/definitely-not-a-model@observed-2026-08-23",
	} {
		if _, err := loaded.Resolve(contract.ModelReference(absent), at); err == nil {
			t.Errorf("%q resolves, and nothing in the snapshot declares it", absent)
		}
	}
}
