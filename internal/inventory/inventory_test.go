package inventory_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/inventory"
)

const twoRevisions = `{"deployments":[
  {"deploymentId":"dep_old","provider":"openai","modelReference":"openai/gpt-5@2026-01-01",
   "upstreamModelId":"gpt-5-2026-01-01","region":"us-east-1","current":false},
  {"deploymentId":"dep_new","provider":"openai","modelReference":"openai/gpt-5@2026-05-01",
   "upstreamModelId":"gpt-5-2026-05-01","region":"us-east-1","current":true}
]}`

func TestResolvesAPinnedReferenceToExactlyThoseWeights(t *testing.T) {
	inv, err := inventory.Parse([]byte(twoRevisions))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	route, err := inv.Resolve("openai/gpt-5@2026-01-01")
	if err != nil {
		t.Fatalf("resolving a pinned reference: %v", err)
	}
	if route.UpstreamModelID != "gpt-5-2026-01-01" {
		t.Errorf("a pinned reference resolved to upstream model %q", route.UpstreamModelID)
	}
	if route.DeploymentID != "dep_old" {
		t.Errorf("a pinned reference resolved to deployment %q", route.DeploymentID)
	}
}

// TestAPinnedReferenceIsNeverSubstituted is the invariant the whole platform
// turns on: a request that named specific weights is served or refused.
// Silently falling back to the current revision is the exact failure the
// contract calls out, and it would look like a successful request.
func TestAPinnedReferenceIsNeverSubstituted(t *testing.T) {
	inv, err := inventory.Parse([]byte(twoRevisions))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	route, err := inv.Resolve("openai/gpt-5@2025-12-31")
	if err == nil {
		t.Fatalf("a revision that is not deployed resolved to %q", route.ModelReference)
	}
	var noRoute inventory.ErrNoRoute
	if !errors.As(err, &noRoute) {
		t.Fatalf("refused with %v, expected ErrNoRoute", err)
	}
}

func TestResolvesAnUnpinnedReferenceToTheCurrentRevision(t *testing.T) {
	inv, err := inventory.Parse([]byte(twoRevisions))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	route, err := inv.Resolve("openai/gpt-5")
	if err != nil {
		t.Fatalf("resolving an unpinned reference: %v", err)
	}
	if route.ModelReference != contract.ModelReference("openai/gpt-5@2026-05-01") {
		t.Errorf("an unpinned reference resolved to %q, expected the current revision", route.ModelReference)
	}
	// The start event has to report a revision-pinned reference, so a route
	// that resolved to a model line would produce a stream the contract cannot
	// describe.
	if !route.ModelReference.Pinned() {
		t.Error("the resolved reference is not revision-pinned")
	}
}

func TestParseRefusesWhatResolutionWouldHaveToGuessAbout(t *testing.T) {
	cases := []struct {
		name     string
		document string
		expect   string
	}{
		{
			name:     "an empty inventory",
			document: `{"deployments":[]}`,
			expect:   "no deployments",
		},
		{
			name: "a deployment that does not pin a revision",
			// It would silently serve whatever the provider's alias points at
			// today, and change meaning without any change here.
			document: `{"deployments":[{"deploymentId":"d","provider":"openai","modelReference":"openai/gpt-5","upstreamModelId":"gpt-5"}]}`,
			expect:   "must pin an immutable revision",
		},
		{
			name:     "a deployment with no upstream model id",
			document: `{"deployments":[{"deploymentId":"d","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":""}]}`,
			expect:   "declares no upstream model id",
		},
		{
			name: "two deployments of the same revision",
			// Legitimate once same-model failover exists; today it would mean
			// picking one silently.
			document: `{"deployments":[
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":"x"},
			  {"deploymentId":"b","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":"y"}]}`,
			expect: "same-model failover is not implemented",
		},
		{
			name: "two current revisions of one model",
			document: `{"deployments":[
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r1","upstreamModelId":"x","current":true},
			  {"deploymentId":"b","provider":"openai","modelReference":"openai/gpt-5@r2","upstreamModelId":"y","current":true}]}`,
			expect: "are both current",
		},
		{
			name: "two deployments sharing an id",
			document: `{"deployments":[
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r1","upstreamModelId":"x"},
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r2","upstreamModelId":"y"}]}`,
			expect: "share the id",
		},
		{
			name:     "a provider slug that is not a slug",
			document: `{"deployments":[{"deploymentId":"d","provider":"OpenAI Inc","modelReference":"openai/gpt-5@r","upstreamModelId":"x"}]}`,
			expect:   "not a provider slug",
		},
		{
			name:     "a model reference that is not a model reference",
			document: `{"deployments":[{"deploymentId":"d","provider":"openai","modelReference":"gpt-5","upstreamModelId":"x"}]}`,
			expect:   "not a model reference",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := inventory.Parse([]byte(testCase.document))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase.expect) {
				t.Errorf("refused with %q, expected it to mention %q", err, testCase.expect)
			}
		})
	}
}

func TestProvidersListsEveryRoutableProvider(t *testing.T) {
	inv, err := inventory.Parse([]byte(`{"deployments":[
	  {"deploymentId":"a","provider":"together","modelReference":"meta/llama@r","upstreamModelId":"x"},
	  {"deploymentId":"b","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":"y"}]}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	got := inv.Providers()
	if len(got) != 2 || got[0] != "openai" || got[1] != "together" {
		t.Errorf("Providers returned %v, expected a sorted [openai together]", got)
	}
}
