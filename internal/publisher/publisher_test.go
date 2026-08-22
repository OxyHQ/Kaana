package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// fakeUpstream serves a provider's REAL wire shape for `GET /v1/models`: the
// OpenAI-compatible list envelope, behind a bearer credential. A fake speaking
// anything else would test the publisher against a shape no provider sends.
type fakeUpstream struct {
	server *httptest.Server
	apiKey string

	mu     sync.Mutex
	models []string
	calls  int
	status int
}

func newFakeUpstream(t *testing.T, apiKey string, models ...string) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{apiKey: apiKey, models: models, status: http.StatusOK}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		upstream.calls++

		if r.URL.Path != "/v1/models" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+upstream.apiKey {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		if upstream.status != http.StatusOK {
			http.Error(w, `{"error":{"message":"upstream is unwell"}}`, upstream.status)
			return
		}

		entries := make([]map[string]any, 0, len(upstream.models))
		for _, id := range upstream.models {
			entries = append(entries, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "unknown"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": entries})
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (f *fakeUpstream) baseURL() string { return f.server.URL + "/v1" }

func (f *fakeUpstream) setModels(models ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = models
}

func (f *fakeUpstream) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeStore is an in-memory object store that can be made to fail its READ
// independently of its write, because those two failures have opposite correct
// responses and the code has to tell them apart.
type fakeStore struct {
	mu        sync.Mutex
	body      []byte
	published bool
	writes    [][]byte
	readErr   error
	writeErr  error
}

func (s *fakeStore) Get(context.Context) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, false, s.readErr
	}
	return s.body, s.published, nil
}

func (s *fakeStore) Put(_ context.Context, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.body = append([]byte(nil), body...)
	s.published = true
	s.writes = append(s.writes, append([]byte(nil), body...))
	return nil
}

func (s *fakeStore) Describe() string { return "memory://snapshot" }

func (s *fakeStore) written() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func testAttribution(t *testing.T) *Attribution {
	t.Helper()
	table, err := ParseAttribution([]byte(`{
	  "attribution": {
	    "cerebras": {"gpt-oss-120b": "openai/gpt-oss-120b", "gemma-4-31b": "google/gemma-4-31b"},
	    "groq":     {"gpt-oss-120b": "openai/gpt-oss-120b"}
	  }
	}`))
	if err != nil {
		t.Fatalf("parsing the test attribution table: %v", err)
	}
	return table
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// parseSnapshot reads a published body back as the reader sees it.
func parseSnapshot(t *testing.T, body []byte) snapshotFile {
	t.Helper()
	var parsed snapshotFile
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("the published snapshot is not readable: %v", err)
	}
	return parsed
}

// TestTheSnapshotIsReIssuedWithAFreshIssuedAtWhenNothingChanged is THE
// requirement `inventory.Store` places on a publisher, and the one a snapshot
// written once satisfies for exactly one horizon before degrading.
//
// It asserts the pair together: `issuedAt` MOVED while the routing content did
// NOT. Asserting only the first would pass for a publisher that re-derives
// everything every cycle; asserting only the second would pass for a publisher
// that has stopped.
func TestTheSnapshotIsReIssuedWithAFreshIssuedAtWhenNothingChanged(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b", "gemma-4-31b")
	store := &fakeStore{}

	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Now:         func() time.Time { return clock },
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}

	ctx := context.Background()
	if err := publisher.PublishOnce(ctx); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Nothing about the providers changes; only time passes.
	clock = clock.Add(15 * time.Minute)
	if err := publisher.PublishOnce(ctx); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	written := store.written()
	if len(written) != 2 {
		t.Fatalf("expected two publishes, got %d", len(written))
	}
	first, second := parseSnapshot(t, written[0]), parseSnapshot(t, written[1])

	if first.IssuedAt == second.IssuedAt {
		t.Errorf("issuedAt was not re-stamped: both snapshots say %q, which Kaana cannot tell from a publisher that has stopped", first.IssuedAt)
	}
	if want := contract.NewTimestamp(clock); second.IssuedAt != want {
		t.Errorf("the re-issued snapshot is stamped %q, want %q", second.IssuedAt, want)
	}
	if first.SnapshotID != second.SnapshotID {
		t.Errorf("the routing content changed across an unchanged re-issue: %q then %q", first.SnapshotID, second.SnapshotID)
	}
	if len(second.Deployments) != len(first.Deployments) {
		t.Errorf("deployment count moved across an unchanged re-issue: %d then %d", len(first.Deployments), len(second.Deployments))
	}
}

// TestAReIssuedSnapshotIsFreshEnoughForTheReader closes the loop the previous
// test opens: a moved `issuedAt` is only useful if the reader then considers
// the snapshot fresh. This reads the published bytes with the REAL reader at
// the instant they were issued and asserts unpinned resolution still works.
func TestAReIssuedSnapshotIsFreshEnoughForTheReader(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b")
	store := &fakeStore{}

	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Now:         func() time.Time { return clock },
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	loaded, err := inventory.Parse(store.written()[0], inventory.DefaultMaxSnapshotAge)
	if err != nil {
		t.Fatalf("the real reader refused the published snapshot: %v", err)
	}

	// One cadence later the snapshot is still well inside the horizon, which is
	// the margin DefaultInterval buys.
	if !loaded.ServesUnpinned(clock.Add(DefaultInterval)) {
		t.Error("a snapshot one cadence old already refuses unpinned references")
	}
	// And past the horizon it correctly stops — the control proving the check
	// above is measuring freshness at all and not a constant true.
	if loaded.ServesUnpinned(clock.Add(inventory.DefaultMaxSnapshotAge + time.Minute)) {
		t.Error("a snapshot past the horizon still claims to serve unpinned references")
	}
}

// TestTheDefaultCadenceLeavesRoomInsideTheHorizon pins the relationship the
// whole design rests on. A cadence equal to the horizon is indistinguishable
// from expired; this asserts a real margin, expressed as consecutive failures
// survivable rather than as a bare inequality.
func TestTheDefaultCadenceLeavesRoomInsideTheHorizon(t *testing.T) {
	if DefaultInterval >= inventory.DefaultMaxSnapshotAge {
		t.Fatalf("the default cadence %s is not shorter than the horizon %s", DefaultInterval, inventory.DefaultMaxSnapshotAge)
	}
	if survivable := int(inventory.DefaultMaxSnapshotAge / DefaultInterval); survivable < 3 {
		t.Errorf("the default cadence survives only %d consecutive failed publishes before unpinned resolution degrades; want at least 3", survivable)
	}
}

// TestACadenceAtOrPastTheHorizonIsRefused proves the guard is real rather than
// documented, and that it refuses instead of quietly clamping.
func TestACadenceAtOrPastTheHorizonIsRefused(t *testing.T) {
	for _, interval := range []time.Duration{inventory.DefaultMaxSnapshotAge, inventory.DefaultMaxSnapshotAge + time.Minute} {
		_, err := New(Config{
			Providers:   []Provider{{Slug: "cerebras", BaseURL: "https://example.invalid/v1", APIKey: "key"}},
			Attribution: testAttribution(t),
			Store:       &fakeStore{},
			Interval:    interval,
			Logger:      quietLogger(),
		})
		if err == nil {
			t.Errorf("a cadence of %s was accepted; every snapshot would age out before its replacement arrived", interval)
		}
	}
	// The control: a cadence inside the horizon is accepted, so the check above
	// is measuring the horizon and not refusing everything.
	if _, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: "https://example.invalid/v1", APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       &fakeStore{},
		Interval:    inventory.DefaultMaxSnapshotAge - time.Minute,
		Logger:      quietLogger(),
	}); err != nil {
		t.Errorf("a cadence inside the horizon was refused: %v", err)
	}
}

// TestARevisionIsCarriedForwardRatherThanReDated is the silent-substitution
// guard. A publisher that stamped today's date every cycle would re-point every
// reference a customer has pinned, daily, with everything green.
func TestARevisionIsCarriedForwardRatherThanReDated(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b")
	store := &fakeStore{}

	clock := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Now:         func() time.Time { return clock },
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}

	ctx := context.Background()
	if err := publisher.PublishOnce(ctx); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Nine days later, same model, same provider.
	clock = clock.AddDate(0, 0, 9)
	if err := publisher.PublishOnce(ctx); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	written := store.written()
	first, second := parseSnapshot(t, written[0]), parseSnapshot(t, written[1])
	if first.Deployments[0].ModelReference != second.Deployments[0].ModelReference {
		t.Errorf("the reference was re-dated across nine days: %q became %q; every customer pin to the first now resolves to nothing",
			first.Deployments[0].ModelReference, second.Deployments[0].ModelReference)
	}
	if want := contract.ModelReference("openai/gpt-oss-120b@observed-2026-08-19"); first.Deployments[0].ModelReference != want {
		t.Errorf("first observation minted %q, want %q", first.Deployments[0].ModelReference, want)
	}

	// The control: a model line seen for the FIRST time on the later date does
	// take that date, so the carry-forward is not simply freezing everything.
	upstream.setModels("gpt-oss-120b", "gemma-4-31b")
	if err := publisher.PublishOnce(ctx); err != nil {
		t.Fatalf("third publish: %v", err)
	}
	third := parseSnapshot(t, store.written()[2])
	var gemma contract.ModelReference
	for _, deployment := range third.Deployments {
		if strings.HasPrefix(string(deployment.ModelReference), "google/gemma-4-31b@") {
			gemma = deployment.ModelReference
		}
	}
	if want := contract.ModelReference("google/gemma-4-31b@observed-2026-08-28"); gemma != want {
		t.Errorf("a newly observed line minted %q, want %q", gemma, want)
	}
}

// TestAFailedReadOfThePreviousSnapshotRefusesToPublish separates the two ways
// there can be no previous snapshot. Absent means first run; unreadable means
// the history exists and could not be seen, and publishing then would re-date
// every reference on the strength of a transient error.
func TestAFailedReadOfThePreviousSnapshotRefusesToPublish(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b")
	store := &fakeStore{readErr: fmt.Errorf("s3 is having a moment")}

	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("a failed read of the previous snapshot still published; every reference would have been re-dated")
	}
	if len(store.written()) != 0 {
		t.Error("a snapshot was written despite the previous one being unreadable")
	}

	// The control: with the read merely EMPTY rather than failing, the same
	// publisher publishes. Without this, the assertion above would also pass
	// for a publisher that never publishes at all.
	store.mu.Lock()
	store.readErr = nil
	store.mu.Unlock()
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("a first run with no previous snapshot refused to publish: %v", err)
	}
	if len(store.written()) != 1 {
		t.Errorf("expected one publish on the first run, got %d", len(store.written()))
	}
}

// TestAnUnattributedModelIsDroppedNotGuessed proves the publisher never invents
// a publisher namespace from a model id — a claim about somebody else's work,
// made on the strength of a substring.
func TestAnUnattributedModelIsDroppedNotGuessed(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b", "some-unheard-of-model")
	store := &fakeStore{}

	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	published := parseSnapshot(t, store.written()[0])
	if len(published.Deployments) != 1 {
		t.Fatalf("expected the unattributed model to be dropped, got %d deployments", len(published.Deployments))
	}
	for _, deployment := range published.Deployments {
		if deployment.UpstreamModelID == "some-unheard-of-model" {
			t.Errorf("an unattributed model reached the snapshot as %q", deployment.ModelReference)
		}
	}
}

// TestTwoProvidersOfOneLineShareOneReferenceAndTheFirstDeclaredLeads is the
// failover shape AND the ordering requirement in one case.
//
// Failover is off by default, so a reference resolves to the deployment
// declared FIRST. Two providers of one model line must therefore produce ONE
// reference with two endpoints, led by the provider declared first — not two
// references, which `inventory.Parse` refuses as two current revisions.
func TestTwoProvidersOfOneLineShareOneReferenceAndTheFirstDeclaredLeads(t *testing.T) {
	cerebras := newFakeUpstream(t, "c-key", "gpt-oss-120b")
	groq := newFakeUpstream(t, "g-key", "gpt-oss-120b")
	store := &fakeStore{}

	publisher, err := New(Config{
		Providers: []Provider{
			{Slug: "groq", BaseURL: groq.baseURL(), APIKey: "g-key"},
			{Slug: "cerebras", BaseURL: cerebras.baseURL(), APIKey: "c-key"},
		},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      groq.server.Client(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	body := store.written()[0]
	loaded, err := inventory.Parse(body, inventory.DefaultMaxSnapshotAge)
	if err != nil {
		t.Fatalf("the real reader refused a two-provider snapshot: %v", err)
	}

	set, err := loaded.Resolve("openai/gpt-oss-120b", time.Now())
	if err != nil {
		t.Fatalf("resolving the shared line: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("two providers of one line produced %d endpoints, want 2", set.Len())
	}
	if first := set.Candidates()[0].Provider; first != "groq" {
		t.Errorf("the primary route is %q; the provider declared FIRST was groq, and failover is off by default", first)
	}
}

// TestOneProviderFailingDoesNotWithdrawTheOthers: withdrawing every reference
// because one upstream is throttling is a larger outage than the one provider's
// absence, and it would arrive as references that simply stop resolving.
func TestOneProviderFailingDoesNotWithdrawTheOthers(t *testing.T) {
	cerebras := newFakeUpstream(t, "c-key", "gemma-4-31b")
	groq := newFakeUpstream(t, "g-key", "gpt-oss-120b")
	groq.fail(http.StatusTooManyRequests)
	store := &fakeStore{}

	publisher, err := New(Config{
		Providers: []Provider{
			{Slug: "groq", BaseURL: groq.baseURL(), APIKey: "g-key"},
			{Slug: "cerebras", BaseURL: cerebras.baseURL(), APIKey: "c-key"},
		},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      groq.server.Client(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing with one provider down: %v", err)
	}

	published := parseSnapshot(t, store.written()[0])
	if len(published.Deployments) != 1 || published.Deployments[0].Provider != "cerebras" {
		t.Fatalf("expected the healthy provider's single route, got %+v", published.Deployments)
	}
}

// TestEveryProviderFailingLeavesThePublishedSnapshotAlone: a cycle with nothing
// to say must not overwrite a good snapshot with an empty one, which Kaana
// would refuse and then keep serving its last good one anyway — with a
// permanently lit reload error.
func TestEveryProviderFailingLeavesThePublishedSnapshotAlone(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b")
	store := &fakeStore{}
	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "cerebras", BaseURL: upstream.baseURL(), APIKey: "key"}},
		Attribution: testAttribution(t),
		Store:       store,
		Client:      upstream.server.Client(),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	upstream.fail(http.StatusInternalServerError)
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("a cycle in which no provider answered reported success")
	}
	if len(store.written()) != 1 {
		t.Errorf("the published snapshot was overwritten by a cycle with nothing to publish: %d writes", len(store.written()))
	}
}

// TestAKeylessProviderIsNeverAsked: an unauthenticated model list is a public
// catalogue at several providers, much larger than the account can call, so
// publishing from one would declare routes that 404 on first use.
func TestAKeylessProviderIsNeverAsked(t *testing.T) {
	upstream := newFakeUpstream(t, "key", "gpt-oss-120b")
	if _, err := Discover(context.Background(), upstream.server.Client(), Provider{
		Slug:    "cerebras",
		BaseURL: upstream.baseURL(),
	}); err == nil {
		t.Fatal("a provider with no credential was asked for its model list")
	}
	if upstream.callCount() != 0 {
		t.Errorf("a keyless provider was contacted %d times", upstream.callCount())
	}
}

// TestTheCredentialNeverReachesAnError is the secrets rule at this boundary:
// the key is on the request, and a provider's error routinely echoes it back.
func TestTheCredentialNeverReachesAnError(t *testing.T) {
	const key = "sk-a-very-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The control: the upstream really does echo the credential, so an
		// assertion that it is absent is measuring redaction and not a quiet
		// upstream.
		http.Error(w, `{"error":{"message":"bad key `+r.Header.Get("Authorization")+`"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := Discover(context.Background(), server.Client(), Provider{
		Slug: "cerebras", BaseURL: server.URL + "/v1", APIKey: key,
	})
	if err == nil {
		t.Fatal("expected the refusal to be reported")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the credential reached the error text: %s", err)
	}
}
