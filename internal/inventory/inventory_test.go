package inventory_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// issued renders a snapshot header stamped at a given moment. Fixtures are
// written RELATIVE to now, never at an absolute date: a pinned date in a
// committed fixture is a staleness test that starts failing on its own.
func issued(at time.Time, deployments string) []byte {
	return fmt.Appendf(nil, `{"snapshotId":"snap_test","issuedAt":%q,"deployments":[%s]}`,
		contract.NewTimestamp(at), deployments)
}

const twoRevisions = `
  {"deploymentId":"dep_old","provider":"openai","modelReference":"openai/gpt-5@2026-01-01",
   "upstreamModelId":"gpt-5-2026-01-01","region":"us-east-1","current":false},
  {"deploymentId":"dep_new","provider":"openai","modelReference":"openai/gpt-5@2026-05-01",
   "upstreamModelId":"gpt-5-2026-05-01","region":"us-east-1","current":true}`

// twoDeploymentsOfOneRevision is the failover set: the same weights, two places
// to get them.
const twoDeploymentsOfOneRevision = `
  {"deploymentId":"dep_primary","provider":"openai","modelReference":"openai/gpt-5@2026-05-01",
   "upstreamModelId":"gpt-5-2026-05-01","region":"us-east-1","current":true},
  {"deploymentId":"dep_secondary","provider":"together","modelReference":"openai/gpt-5@2026-05-01",
   "upstreamModelId":"openai/gpt-5-2026-05-01","region":"us-west-2","current":true}`

func parse(t *testing.T, document []byte) *inventory.Inventory {
	t.Helper()
	parsed, err := inventory.Parse(document, time.Hour)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	return parsed
}

func TestResolvesAPinnedReferenceToExactlyThoseWeights(t *testing.T) {
	now := time.Now()
	set, err := parse(t, issued(now, twoRevisions)).Resolve("openai/gpt-5@2026-01-01", now)
	if err != nil {
		t.Fatalf("resolving a pinned reference: %v", err)
	}
	candidates := set.Candidates()
	if len(candidates) != 1 {
		t.Fatalf("a pinned reference resolved to %d candidates", len(candidates))
	}
	if candidates[0].UpstreamModelID != "gpt-5-2026-01-01" {
		t.Errorf("a pinned reference resolved to upstream model %q", candidates[0].UpstreamModelID)
	}
	if candidates[0].DeploymentID != "dep_old" {
		t.Errorf("a pinned reference resolved to deployment %q", candidates[0].DeploymentID)
	}
}

// TestAPinnedReferenceIsNeverSubstituted is the invariant the whole platform
// turns on: a request that named specific weights is served or refused.
// Silently falling back to the current revision is the exact failure the
// contract calls out, and it would look like a successful request.
func TestAPinnedReferenceIsNeverSubstituted(t *testing.T) {
	now := time.Now()
	set, err := parse(t, issued(now, twoRevisions)).Resolve("openai/gpt-5@2025-12-31", now)
	if err == nil {
		t.Fatalf("a revision that is not deployed resolved to %q", set.Reference())
	}
	var noRoute inventory.ErrNoRoute
	if !errors.As(err, &noRoute) {
		t.Fatalf("refused with %v, expected ErrNoRoute", err)
	}
}

func TestResolvesAnUnpinnedReferenceToTheCurrentRevision(t *testing.T) {
	now := time.Now()
	set, err := parse(t, issued(now, twoRevisions)).Resolve("openai/gpt-5", now)
	if err != nil {
		t.Fatalf("resolving an unpinned reference: %v", err)
	}
	if set.Reference() != contract.ModelReference("openai/gpt-5@2026-05-01") {
		t.Errorf("an unpinned reference resolved to %q, expected the current revision", set.Reference())
	}
	// The start event has to report a revision-pinned reference, so a route
	// that resolved to a model line would produce a stream the contract cannot
	// describe.
	if !set.Reference().Pinned() {
		t.Error("the resolved reference is not revision-pinned")
	}
}

/* -------------------------------------------------------------------------- */
/*  Same-model failover sets                                                  */
/* -------------------------------------------------------------------------- */

// TestOneRevisionResolvesToEveryDeploymentServingIt is the shape failover needs:
// several places, one set of weights.
func TestOneRevisionResolvesToEveryDeploymentServingIt(t *testing.T) {
	now := time.Now()
	set, err := parse(t, issued(now, twoDeploymentsOfOneRevision)).Resolve("openai/gpt-5@2026-05-01", now)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("the set holds %d endpoints, expected both deployments of the revision", set.Len())
	}

	candidates := set.Candidates()
	if candidates[0].DeploymentID != "dep_primary" || candidates[1].DeploymentID != "dep_secondary" {
		t.Errorf("candidates arrived as %q then %q; the inventory's declared order is the tie-break",
			candidates[0].DeploymentID, candidates[1].DeploymentID)
	}
	if candidates[0].Provider == candidates[1].Provider {
		t.Error("the fixture is meant to span two providers, or it does not exercise failover across one")
	}
	if candidates[0].UpstreamModelID == candidates[1].UpstreamModelID {
		t.Error("the fixture is meant to give each provider its own upstream model id")
	}
}

// TestEveryCandidateOfASetNamesTheSameWeights is the same-model guarantee, read
// off the values a route resolution actually produces.
//
// The structural half of the guarantee is that RouteSet holds ONE reference for
// the whole set and an Endpoint holds none — see the test below — so this check
// is what proves the pairing does not undo it.
func TestEveryCandidateOfASetNamesTheSameWeights(t *testing.T) {
	now := time.Now()
	set, err := parse(t, issued(now, twoDeploymentsOfOneRevision)).Resolve("openai/gpt-5", now)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	candidates := set.Candidates()
	if len(candidates) < 2 {
		t.Fatalf("the set produced %d candidates; with fewer than two, sameness across candidates is vacuous", len(candidates))
	}
	for _, candidate := range candidates {
		if candidate.ModelReference != set.Reference() {
			t.Errorf("candidate %s serves %q, and the set resolved %q: failing over to it would substitute the model",
				candidate.DeploymentID, candidate.ModelReference, set.Reference())
		}
	}
}

// TestAnEndpointCannotCarryItsOwnModelReference gates the structural half of
// the same-model rule by inspecting the type.
//
// A cross-model failover would have to begin with an endpoint that knows which
// model it serves, because that is the only way two candidates in one set could
// differ. Adding such a field is the change this test exists to fail on, and it
// asserts the inspection can SEE such a field by finding one on Deployment,
// which legitimately has it as the inventory file's input shape.
func TestAnEndpointCannotCarryItsOwnModelReference(t *testing.T) {
	referenceTypes := map[reflect.Type]bool{
		reflect.TypeOf(contract.ModelReference("")): true,
		reflect.TypeOf(contract.ModelID("")):        true,
	}

	endpoint := reflect.TypeOf(inventory.Endpoint{})
	for index := range endpoint.NumField() {
		field := endpoint.Field(index)
		if referenceTypes[field.Type] {
			t.Errorf("Endpoint.%s is a %s: an endpoint that names its own model is one a route set could pair with different weights",
				field.Name, field.Type)
		}
	}

	// Positive control: the same inspection must find the field on the type
	// that does have one, or "Endpoint has no model reference" is also what a
	// broken type comparison reports.
	found := false
	deployment := reflect.TypeOf(inventory.Deployment{})
	for index := range deployment.NumField() {
		if referenceTypes[deployment.Field(index).Type] {
			found = true
		}
	}
	if !found {
		t.Fatal("the inspection found no model reference on Deployment either, so it is not measuring what it claims to")
	}
}

/* -------------------------------------------------------------------------- */
/*  Snapshot staleness                                                        */
/* -------------------------------------------------------------------------- */

// TestAStaleSnapshotStillServesPinnedReferences is the whole of what a
// configuration snapshot may serve during a control-plane outage.
//
// Pinned weights are an immutable mapping: nothing about
// `openai/gpt-5@2026-05-01 → gpt-5-2026-05-01 on openai` can have gone stale, so
// refusing it would be an outage Kaana inflicted on itself for no gain.
func TestAStaleSnapshotStillServesPinnedReferences(t *testing.T) {
	issuedAt := time.Now().Add(-24 * time.Hour)
	parsed, err := inventory.Parse(issued(issuedAt, twoRevisions), time.Hour)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	set, err := parsed.Resolve("openai/gpt-5@2026-05-01", time.Now())
	if err != nil {
		t.Fatalf("a day-old snapshot refused a pinned reference: %v", err)
	}
	if set.Reference() != "openai/gpt-5@2026-05-01" {
		t.Errorf("resolved %q", set.Reference())
	}
}

// TestAStaleSnapshotRefusesToChooseACurrentRevision is the other half: which
// revision is current is Oxy's decision and it is the one thing in the file
// that decays. Answering from a snapshot nobody is re-issuing would serve
// weights that Oxy may have replaced hours ago, on a decision nobody made.
func TestAStaleSnapshotRefusesToChooseACurrentRevision(t *testing.T) {
	issuedAt := time.Now().Add(-90 * time.Minute)
	parsed, err := inventory.Parse(issued(issuedAt, twoRevisions), time.Hour)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	// The control: the same snapshot resolves the same reference while it is
	// inside the horizon, so the refusal below is the horizon and not the
	// fixture.
	if _, err := parsed.Resolve("openai/gpt-5", issuedAt.Add(time.Minute)); err != nil {
		t.Fatalf("a one-minute-old snapshot refused an unpinned reference: %v", err)
	}

	_, err = parsed.Resolve("openai/gpt-5", time.Now())
	var stale inventory.ErrSnapshotTooStale
	if !errors.As(err, &stale) {
		t.Fatalf("a 90-minute-old snapshot resolved an unpinned reference under a 1h horizon: %v", err)
	}
	if stale.Age < time.Hour {
		t.Errorf("the refusal reports an age of %s", stale.Age)
	}
	if parsed.ServesUnpinned(time.Now()) {
		t.Error("the health projection still reports unpinned references as served")
	}
}

/* -------------------------------------------------------------------------- */
/*  Parse refusals                                                            */
/* -------------------------------------------------------------------------- */

func TestParseRefusesWhatResolutionWouldHaveToGuessAbout(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		document []byte
		expect   string
	}{
		{
			name:     "an empty inventory",
			document: issued(now, ``),
			expect:   "no deployments",
		},
		{
			name: "a deployment that does not pin a revision",
			// It would silently serve whatever the provider's alias points at
			// today, and change meaning without any change here.
			document: issued(now, `{"deploymentId":"d","provider":"openai","modelReference":"openai/gpt-5","upstreamModelId":"gpt-5"}`),
			expect:   "must pin an immutable revision",
		},
		{
			name:     "a deployment with no upstream model id",
			document: issued(now, `{"deploymentId":"d","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":""}`),
			expect:   "declares no upstream model id",
		},
		{
			name: "two current revisions of one model",
			document: issued(now, `
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r1","upstreamModelId":"x","current":true},
			  {"deploymentId":"b","provider":"openai","modelReference":"openai/gpt-5@r2","upstreamModelId":"y","current":true}`),
			expect: "are both current",
		},
		{
			name: "two deployments sharing an id",
			document: issued(now, `
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r1","upstreamModelId":"x"},
			  {"deploymentId":"a","provider":"openai","modelReference":"openai/gpt-5@r2","upstreamModelId":"y"}`),
			expect: "share the id",
		},
		{
			name:     "a provider slug that is not a slug",
			document: issued(now, `{"deploymentId":"d","provider":"OpenAI Inc","modelReference":"openai/gpt-5@r","upstreamModelId":"x"}`),
			expect:   "not a provider slug",
		},
		{
			name:     "a model reference that is not a model reference",
			document: issued(now, `{"deploymentId":"d","provider":"openai","modelReference":"gpt-5","upstreamModelId":"x"}`),
			expect:   "not a model reference",
		},
		{
			name: "a snapshot that does not say when it was issued",
			// Its staleness could only be guessed at, and the guess that keeps
			// serving is the one that silently serves a revision Oxy replaced.
			document: []byte(`{"snapshotId":"s","deployments":[
			  {"deploymentId":"d","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":"x"}]}`),
			expect: "declares no issuedAt",
		},
		{
			name: "a snapshot with no id",
			document: []byte(fmt.Sprintf(`{"issuedAt":%q,"deployments":[
			  {"deploymentId":"d","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":"x"}]}`, contract.NewTimestamp(now))),
			expect: "declares no snapshotId",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := inventory.Parse(testCase.document, time.Hour)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase.expect) {
				t.Errorf("refused with %q, expected it to mention %q", err, testCase.expect)
			}
		})
	}
}

// TestAHorizonOfZeroIsRefused: a zero horizon reads as "never stale", which is
// the one value an operator must not reach by leaving a field empty.
func TestAHorizonOfZeroIsRefused(t *testing.T) {
	_, err := inventory.Parse(issued(time.Now(), twoRevisions), 0)
	if err == nil {
		t.Fatal("a zero staleness horizon was accepted")
	}
	if !strings.Contains(err.Error(), "valid forever") {
		t.Errorf("refused with %q", err)
	}
}

// TestTwoDeploymentsOfOneRevisionAreAcceptedNow records the rule this change
// replaced: the previous build refused a second deployment of one reference
// because it had no way to choose between them.
func TestTwoDeploymentsOfOneRevisionAreAcceptedNow(t *testing.T) {
	if _, err := inventory.Parse(issued(time.Now(), twoDeploymentsOfOneRevision), time.Hour); err != nil {
		t.Fatalf("a failover set was refused: %v", err)
	}
}

func TestProvidersListsEveryRoutableProvider(t *testing.T) {
	parsed := parse(t, issued(time.Now(), `
	  {"deploymentId":"a","provider":"together","modelReference":"meta/llama@r","upstreamModelId":"x"},
	  {"deploymentId":"b","provider":"openai","modelReference":"openai/gpt-5@r","upstreamModelId":"y"}`))

	got := parsed.Providers()
	if len(got) != 2 || got[0] != "openai" || got[1] != "together" {
		t.Errorf("Providers returned %v, expected a sorted [openai together]", got)
	}
	if len(parsed.Deployments()) != 2 {
		t.Errorf("Deployments returned %d endpoints, expected 2", len(parsed.Deployments()))
	}
}
