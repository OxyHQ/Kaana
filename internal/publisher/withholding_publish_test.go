package publisher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// fakeEvidence answers PublicationEvidence from memory, or fails.
type fakeEvidence struct {
	mu       sync.Mutex
	evidence Evidence
	err      error
	since    time.Time
}

func (f *fakeEvidence) PublicationEvidence(_ context.Context, _ []contract.ProviderSlug, since time.Time) (Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since = since
	return f.evidence, f.err
}

func (f *fakeEvidence) set(evidence Evidence, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evidence, f.err = evidence, err
}

const (
	cerebrasGPT   contract.DeploymentID = "dep_cerebras_gpt_oss_120b_observed_2026_08_19"
	cerebrasGemma contract.DeploymentID = "dep_cerebras_gemma_4_31b_observed_2026_08_19"
)

func unfundedCerebras(at time.Time) Evidence {
	evidence := Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{"cerebras": {{KeyID: "key-c"}}}}
	evidence.Failures = []FailureStreak{{
		DeploymentID: cerebrasGPT, Provider: "cerebras", KeyID: "key-c", Code: contract.CodeProviderBillingRefused,
		Count: 1, First: at.Add(-time.Minute), Last: at.Add(-time.Minute),
	}}
	return evidence
}

func withholdingPublisher(t *testing.T, store *fakeStore, evidence EvidenceSource, clock *time.Time, reportOnly bool) *Publisher {
	t.Helper()
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b", "gemma-4-31b")
	policy := DefaultWithholdPolicy()
	policy.ReportOnly = reportOnly
	inventoryPublisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Now:         func() time.Time { return *clock },
		Logger:      quietLogger(),
		Evidence:    evidence,
		Withholding: policy,
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	return inventoryPublisher
}

// The owner's case end to end: Cerebras lists a model its account cannot pay
// for. The route is absent from what serving reads, named in `withheld`, and
// returns — with its original revision — once the quarantine ends.
func TestAnUnfundedDeploymentIsWithheldAndReturnsWithItsRevision(t *testing.T) {
	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	evidence := &fakeEvidence{}
	inventoryPublisher := withholdingPublisher(t, store, evidence, &clock, false)
	ctx := context.Background()

	// First cycle: nothing known, both published. This is the date to keep.
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	healthy := parseSnapshot(t, store.written()[0])
	if len(healthy.Deployments) != 2 || len(healthy.Withheld) != 0 {
		t.Fatalf("positive control: %d deployments, %d withheld", len(healthy.Deployments), len(healthy.Withheld))
	}

	// Days later the account runs dry. Observing the model line on another
	// date is exactly what a lost observation would do on its return.
	clock = clock.Add(72 * time.Hour)
	evidence.set(unfundedCerebras(clock), nil)
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("withholding publish: %v", err)
	}
	withheldBody := store.written()[1]
	withheld := parseSnapshot(t, withheldBody)
	if len(withheld.Deployments) != 1 || withheld.Deployments[0].DeploymentID != cerebrasGemma {
		t.Fatalf("published = %+v; want only the gemma route", withheld.Deployments)
	}
	if len(withheld.Withheld) != 1 || withheld.Withheld[0].DeploymentID != cerebrasGPT ||
		withheld.Withheld[0].Reason != WithheldCredentialRefused || withheld.Withheld[0].ModelReference != "openai/gpt-oss-120b@observed-2026-08-19" {
		t.Fatalf("withheld = %+v", withheld.Withheld)
	}
	if want := contract.NewTimestamp(clock.Add(-time.Minute).Add(DefaultWithholdMinQuarantine)); withheld.Withheld[0].Until != want {
		t.Errorf("until = %s, want %s", withheld.Withheld[0].Until, want)
	}
	loaded, err := inventory.Parse(withheldBody, inventory.DefaultMaxSnapshotAge)
	if err != nil {
		t.Fatalf("the real reader refused a snapshot with a withheld list: %v", err)
	}
	if _, err := loaded.Resolve("openai/gpt-oss-120b", clock); err == nil {
		t.Fatal("serving still resolves the withheld line")
	}
	if !evidence.since.Equal(clock.Add(-DefaultWithholdLookback)) {
		t.Errorf("evidence read since %s, want the lookback", evidence.since)
	}

	// The quarantine ends with no new failure: published for a trial, under
	// the SAME revision it was first observed with.
	clock = clock.Add(DefaultWithholdMinQuarantine)
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("trial publish: %v", err)
	}
	restored := parseSnapshot(t, store.written()[2])
	if len(restored.Deployments) != 2 || len(restored.Withheld) != 0 {
		t.Fatalf("after the quarantine: %d deployments, %d withheld", len(restored.Deployments), len(restored.Withheld))
	}
	if restored.SnapshotID != healthy.SnapshotID {
		t.Errorf("the restored snapshot is %s, want the original routing content %s: the line was re-dated", restored.SnapshotID, healthy.SnapshotID)
	}
}

// Re-stating an unchanged decision must not move snapshotId, even as the
// evidence around it (count, return time) moves.
func TestTheWithheldEvidenceIsNotHashedIntoTheSnapshotID(t *testing.T) {
	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	evidence := &fakeEvidence{}
	evidence.set(unfundedCerebras(clock), nil)
	inventoryPublisher := withholdingPublisher(t, store, evidence, &clock, false)
	ctx := context.Background()
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	clock = clock.Add(15 * time.Minute)
	moved := unfundedCerebras(clock)
	moved.Failures[0].Count = 3
	evidence.set(moved, nil)
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	first, second := parseSnapshot(t, store.written()[0]), parseSnapshot(t, store.written()[1])
	if first.Withheld[0].Until == second.Withheld[0].Until || first.Withheld[0].Failures == second.Withheld[0].Failures {
		t.Fatalf("control: the evidence did not move (%+v, %+v)", first.Withheld[0], second.Withheld[0])
	}
	if first.SnapshotID != second.SnapshotID {
		t.Errorf("snapshotId moved with the evidence: %s then %s", first.SnapshotID, second.SnapshotID)
	}
}

func TestReportOnlyNamesTheDecisionAndWithholdsNothing(t *testing.T) {
	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	discoveries := []Discovery{{Provider: Provider{Slug: "cerebras"}, Models: []DiscoveredModel{{UpstreamModelID: "gpt-oss-120b"}, {UpstreamModelID: "gemma-4-31b"}}}}
	decide := NewDecider(unfundedCerebras(clock), DefaultWithholdPolicy(), clock).Decide
	built, err := BuildSnapshotWithholding(discoveries, testAttribution(t), Observations{}, clock, decide, true)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	if built.Deployments != 2 || len(built.Withheld) != 0 || len(parseSnapshot(t, built.Body).Withheld) != 0 {
		t.Fatalf("report-only withheld something: %d deployments, %+v", built.Deployments, built.Withheld)
	}
	if len(built.WouldWithhold) != 1 || built.WouldWithhold[0].DeploymentID != cerebrasGPT || built.WouldWithhold[0].KeyID != "key-c" {
		t.Fatalf("would withhold = %+v", built.WouldWithhold)
	}
}

// A database blip must neither flap withheld routes back into routing nor
// invent new withholdings: the previous snapshot's unexpired decisions stand.
func TestUnreadableEvidenceKeepsThePreviousUnexpiredWithholdings(t *testing.T) {
	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	evidence := &fakeEvidence{}
	evidence.set(unfundedCerebras(clock), nil)
	inventoryPublisher := withholdingPublisher(t, store, evidence, &clock, false)
	ctx := context.Background()
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	evidence.set(Evidence{}, errors.New("database unavailable"))
	clock = clock.Add(15 * time.Minute)
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("publish with unreadable evidence: %v", err)
	}
	carried := parseSnapshot(t, store.written()[1])
	if len(carried.Deployments) != 1 || len(carried.Withheld) != 1 || carried.Withheld[0].DeploymentID != cerebrasGPT {
		t.Fatalf("carried = %d deployments, withheld %+v", len(carried.Deployments), carried.Withheld)
	}

	// Past its `until`, a carried decision expires like any other.
	clock = clock.Add(time.Hour)
	if err := inventoryPublisher.PublishOnce(ctx); err != nil {
		t.Fatalf("publish past the carried until: %v", err)
	}
	if expired := parseSnapshot(t, store.written()[2]); len(expired.Deployments) != 2 || len(expired.Withheld) != 0 {
		t.Fatalf("an expired carried decision still withholds: %+v", expired.Withheld)
	}
}

func TestEverythingWithheldLeavesThePublishedSnapshotAlone(t *testing.T) {
	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	evidence := &fakeEvidence{}
	evidence.set(Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{"cerebras": {{
		KeyID: "key-c", Retirement: "exhausted", RetiredUntil: clock.Add(time.Hour),
	}}}}, nil)
	inventoryPublisher := withholdingPublisher(t, store, evidence, &clock, false)
	if err := inventoryPublisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("a snapshot with every route withheld was published")
	}
	if len(store.written()) != 0 {
		t.Fatal("something was written")
	}
}
