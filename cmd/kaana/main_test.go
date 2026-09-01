package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// TestAProviderSlugResolvesToItsOwnAdapterAddressAndCredentials.
//
// The inventory names a provider SLUG and holds nothing else about it — no
// address, and above all no credential, which would be a copy of an Oxy entity.
// So the slug is resolved here, and what a reviewer needs to see is that three
// providers speaking ONE protocol are three configurations rather than three
// adapters.
func TestAProviderSlugResolvesToItsOwnAdapterAddressAndPolicy(t *testing.T) {
	environment := map[string]string{
		"KAANA_PROVIDERS": "openai,openrouter,cerebras,anthropic",
		"KAANA_PROVIDER_OPENROUTER_KEYS_ON_SEPARATE_ACCOUNTS": "true",
		"KAANA_PROVIDER_OPENROUTER_KEY_RETIREMENT":            "45m",
		"KAANA_PROVIDER_CEREBRAS_BASE_URL":                    "https://cerebras.example.invalid/v1",
	}
	configs, err := parseProviders(lookup(environment))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	if len(configs) != 4 {
		t.Fatalf("%d providers were configured, expected 4", len(configs))
	}

	byName := make(map[contract.ProviderSlug]providerConfig, len(configs))
	for _, config := range configs {
		byName[config.Slug] = config
	}

	// Three providers, one protocol, three addresses: the claim the whole
	// change rests on. A build that still hardwired one provider would fail
	// here rather than at a customer request.
	for _, slug := range []contract.ProviderSlug{"openai", "openrouter", "cerebras"} {
		if got := byName[slug].Protocol; got != providerconfig.ProtocolOpenAICompatible {
			t.Errorf("%s resolves to protocol %q, expected %q", slug, got, providerconfig.ProtocolOpenAICompatible)
		}
	}
	if byName["anthropic"].Protocol != providerconfig.ProtocolAnthropicMessages {
		t.Errorf("anthropic resolves to protocol %q", byName["anthropic"].Protocol)
	}
	addresses := map[contract.ProviderSlug]string{
		"openai":     "https://api.openai.com/v1",
		"openrouter": "https://openrouter.ai/api/v1",
		"cerebras":   "https://cerebras.example.invalid/v1",
	}
	for slug, expected := range addresses {
		if got := byName[slug].BaseURL; got != expected {
			t.Errorf("%s resolves to %q, expected %q", slug, got, expected)
		}
	}

	openrouter := byName["openrouter"]
	if len(openrouter.Declarations) != 0 {
		t.Fatalf("non-secret provider configuration populated credentials: %v", openrouter.Declarations)
	}
	if !openrouter.Keys.OnSeparateAccounts {
		t.Error("the operator declared openrouter's keys are separate accounts and the policy does not carry it")
	}
	if openrouter.Keys.Retirement != 45*time.Minute {
		t.Errorf("openrouter's retirement window is %s, expected 45m", openrouter.Keys.Retirement)
	}
	// Every configuration above must actually build an adapter reporting the
	// slug it was configured under. A registry keys on the adapter's OWN slug,
	// so a mis-resolution here would surface as a duplicate or as a receipt
	// attributed to the wrong provider.
	for index := range configs {
		configs[index].Declarations = []provider.KeyDeclaration{{KeyID: "test", Secret: "fake-provider-key"}}
	}
	adapters, err := buildAdapters(configs)
	if err != nil {
		t.Fatalf("building the adapters: %v", err)
	}
	built := make(map[contract.ProviderSlug]bool, len(adapters))
	for _, adapter := range adapters {
		built[adapter.Provider()] = true
	}
	for slug := range byName {
		if !built[slug] {
			t.Errorf("no adapter was built reporting the slug %q", slug)
		}
	}
	if _, err := provider.NewRegistry(adapters...); err != nil {
		t.Errorf("the adapters do not form a registry: %v", err)
	}
}

// TestTheProviderConfigurationRefusesWhatItCannotResolve.
//
// Every case is a configuration that would otherwise present as working: a
// provider serving under another provider's name, one reading another's
// credentials, or one with an address this build invented.
func TestTheProviderConfigurationRefusesWhatItCannotResolve(t *testing.T) {
	for name, environment := range map[string]map[string]string{
		"no providers at all": {},
		"a slug that is not a slug": {
			"KAANA_PROVIDERS": "Open AI",
		},
		"the same provider twice": {
			"KAANA_PROVIDERS": "openai,openai",
		},
		// `open-router` and `open.router` are two slugs and one variable name.
		// Left alone, the second would silently be configured with the first
		// one's address and credentials.
		"two slugs reading one variable": {
			"KAANA_PROVIDERS":                     "open-router,open.router",
			"KAANA_PROVIDER_OPEN_ROUTER_BASE_URL": "https://openrouter.example.invalid/v1",
			"KAANA_PROVIDER_OPEN_ROUTER_PROTOCOL": providerconfig.ProtocolOpenAICompatible,
		},
		"a provider this build knows no protocol for": {
			"KAANA_PROVIDERS":                          "unknown-provider",
			"KAANA_PROVIDER_UNKNOWN_PROVIDER_BASE_URL": "https://unknown.example.invalid/v1",
		},
		"a protocol this build does not speak": {
			"KAANA_PROVIDERS":              "groq",
			"KAANA_PROVIDER_GROQ_PROTOCOL": "cohere_v1",
			"KAANA_PROVIDER_GROQ_BASE_URL": "https://groq.example.invalid/v1",
		},
		"a provider this build knows no address for": {
			"KAANA_PROVIDERS":                          "unknown-provider",
			"KAANA_PROVIDER_UNKNOWN_PROVIDER_PROTOCOL": providerconfig.ProtocolOpenAICompatible,
		},
		"a provider credential hidden in the base URL path": {
			"KAANA_PROVIDERS":                          "unknown-provider",
			"KAANA_PROVIDER_UNKNOWN_PROVIDER_PROTOCOL": providerconfig.ProtocolOpenAICompatible,
			"KAANA_PROVIDER_UNKNOWN_PROVIDER_BASE_URL": "https://api.example.invalid/v1/sk-abcdefghijkl",
		},
		// The Messages API adapter reports its slug as a constant, so serving
		// it under another name would attribute every event and every usage
		// record to `anthropic` while the inventory routed elsewhere.
		"the messages api under another provider's name": {
			"KAANA_PROVIDERS":                 "bedrock",
			"KAANA_PROVIDER_BEDROCK_PROTOCOL": providerconfig.ProtocolAnthropicMessages,
			"KAANA_PROVIDER_BEDROCK_BASE_URL": "https://bedrock.example.invalid/v1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProviders(lookup(environment)); err == nil {
				t.Error("the configuration was accepted")
			}
		})
	}

	// The control: the shape all of the above are deviations from is accepted,
	// or a parser that refused everything would pass every case here.
	accepted := map[string]string{
		"KAANA_PROVIDERS":              "groq",
		"KAANA_PROVIDER_GROQ_PROTOCOL": providerconfig.ProtocolOpenAICompatible,
		"KAANA_PROVIDER_GROQ_BASE_URL": "https://groq.example.invalid/v1",
	}
	if _, err := parseProviders(lookup(accepted)); err != nil {
		t.Errorf("a well-formed provider declaration was refused: %v", err)
	}
}

// lookup turns a map into the getenv parseProviders reads, so the whole of it
// is testable without a process environment.
func lookup(environment map[string]string) func(string) string {
	return func(name string) string { return environment[name] }
}

// TestPerProviderHeadersAreReviewedAndNeverCarryACredential.
//
// OpenRouter is served by an adapter whose `Headers` field exists for its
// attribution headers, and a provider wired without them is compliant until it
// is not — which is worse than being wired wrong, because nothing fails until
// the provider decides it should.
func TestPerProviderHeadersAreReviewedAndNeverCarryACredential(t *testing.T) {
	configs, err := parseProviders(lookup(map[string]string{
		"KAANA_PROVIDERS": "openrouter",
	}))
	if err != nil {
		t.Fatalf("the headers were refused: %v", err)
	}
	headers := configs[0].Headers
	if headers["HTTP-Referer"] != "https://oxy.so" || headers["X-Title"] != "Oxy" {
		t.Errorf("the reviewed headers are %v", headers)
	}

	for name, environment := range map[string]map[string]string{
		"an authorization header": {
			"KAANA_PROVIDERS": "openrouter", "KAANA_PROVIDER_OPENROUTER_HEADERS": "Authorization=Bearer fake-key",
		},
		"a provider key header": {
			"KAANA_PROVIDERS": "openrouter", "KAANA_PROVIDER_OPENROUTER_HEADERS": "x-api-key=fake-key",
		},
		"a provider key under another vendor spelling": {
			"KAANA_PROVIDERS": "openrouter", "KAANA_PROVIDER_OPENROUTER_HEADERS": "X-Goog-Api-Key=fake-key",
		},
		"an unreviewed public-looking header": {
			"KAANA_PROVIDERS": "openrouter", "KAANA_PROVIDER_OPENROUTER_HEADERS": "User-Agent=Kaana",
		},
		"a credential in an otherwise public title": {
			"KAANA_PROVIDERS": "openrouter", "KAANA_PROVIDER_OPENROUTER_HEADERS": "X-Title=sk-secret",
		},
		"a credential in URL userinfo": {
			"KAANA_PROVIDERS": "openrouter", "KAANA_PROVIDER_OPENROUTER_HEADERS": "HTTP-Referer=https://sk-secret@oxy.so",
		},
		// The Messages API adapter carries no extra headers, so accepting them
		// would drop them silently — the same outcome as never setting them and
		// harder to see.
		"headers for a protocol that sends none": {
			"KAANA_PROVIDERS": "anthropic", "KAANA_PROVIDER_ANTHROPIC_HEADERS": "X-Title=Oxy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProviders(lookup(environment)); err == nil {
				t.Error("the configuration was accepted")
			}
		})
	}
}

// TestADeclaredFactCannotBeUnsetBySpelling.
//
// `_KEYS_ON_SEPARATE_ACCOUNTS` is the operator stating something the process
// cannot work out for itself — whether a pool's keys are different provider
// accounts — and it is the only thing that lets a throttle rotate. A spelling
// that quietly means "not set" hands them the default while they believe they
// changed it, and the failure shows up as requests failing on a throttle the
// next key would have served. So it is read the way every neighbouring value
// is, and refused when it cannot be read at all.
func TestADeclaredFactCannotBeUnsetBySpelling(t *testing.T) {
	for _, spelling := range []string{"true", " true ", "TRUE", "True"} {
		configs, err := parseProviders(lookup(map[string]string{
			"KAANA_PROVIDERS": "openai",
			"KAANA_PROVIDER_OPENAI_KEYS_ON_SEPARATE_ACCOUNTS": spelling,
		}))
		if err != nil {
			t.Fatalf("%q was refused: %v", spelling, err)
		}
		if !configs[0].Keys.OnSeparateAccounts {
			t.Errorf("%q left the declaration unset, so a throttle would never rotate", spelling)
		}
	}

	// The control: the default really is off, or every case above would pass on
	// a parser that returned true for everything.
	configs, err := parseProviders(lookup(map[string]string{"KAANA_PROVIDERS": "openai"}))
	if err != nil {
		t.Fatalf("an absent declaration was refused: %v", err)
	}
	if configs[0].Keys.OnSeparateAccounts {
		t.Fatal("an unset variable declared separate accounts")
	}
	for _, spelling := range []string{"false", "FALSE", " false "} {
		configs, err := parseProviders(lookup(map[string]string{
			"KAANA_PROVIDERS": "openai",
			"KAANA_PROVIDER_OPENAI_KEYS_ON_SEPARATE_ACCOUNTS": spelling,
		}))
		if err != nil {
			t.Fatalf("%q was refused: %v", spelling, err)
		}
		if configs[0].Keys.OnSeparateAccounts {
			t.Errorf("%q declared separate accounts", spelling)
		}
	}

	// Anything else refuses to start rather than choosing a meaning for it.
	for _, spelling := range []string{"yes", "1", "on", "no"} {
		if _, err := parseProviders(lookup(map[string]string{
			"KAANA_PROVIDERS": "openai",
			"KAANA_PROVIDER_OPENAI_KEYS_ON_SEPARATE_ACCOUNTS": spelling,
		})); err == nil {
			t.Errorf("%q was accepted, and it means whichever of the two an operator assumed", spelling)
		}
	}
}

// TestOneConditionHasOneMessage.
//
// A snapshot routing somewhere this build cannot reach is not fatal on either
// path, so the only instrument for it is a log filter — and a second spelling
// of the same message is one an alarm silently half-misses. Startup and reload
// call the same function; this asserts the message exists exactly once in the
// source, which is what a filter is written against.
func TestOneConditionHasOneMessage(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading the source: %v", err)
	}
	// Anchored on the quote so a mention in a comment is not counted, and
	// stopping before "provider" so a singular variant is a SECOND match rather
	// than an invisible one.
	const message = `"the installed snapshot routes to `
	if count := strings.Count(string(source), message); count != 1 {
		t.Errorf("the message an alarm filters on appears %d times; a second spelling is one the filter misses", count)
	}
	// The control: the string really is there, so a typo in this test cannot
	// report a clean zero as agreement.
	if !strings.Contains(string(source), message+`providers this build has no adapter for"`) {
		t.Error("the message this test filters on is not the one the source emits")
	}

	// The count above is anchored BEFORE the plural, so re-adding the old
	// inline loop is a second match rather than an invisible one — which is
	// what it is mutation-tested against. What it cannot see is a second
	// warning for this condition worded differently enough to miss the anchor,
	// so the condition itself is counted too: there is exactly one place that
	// asks whether a snapshot's provider has an adapter, and re-adding a loop
	// has to ask again.
	//
	// An exact count rather than a floor, and deliberately blunt: a legitimate
	// future use of Lookup here fails this and has to say why, which is the
	// cheapest way to make somebody look at the log the alarm reads.
	if count := strings.Count(string(source), ".Lookup("); count != 1 {
		t.Errorf("the snapshot's providers are checked against the registry in %d places; each one is a warning an alarm has to know about separately", count)
	}

	// And that the two paths still CALL it. Both counts above are satisfied by
	// a build where startup or reload has quietly stopped asking: one spelling
	// of one message, one Lookup, inside a function nobody invokes. Measured —
	// deleting either call site left this test, and the whole package, green.
	//
	// TestTheReloadPathActuallyWarns covers the reload side behaviourally,
	// which is the stronger instrument and the path that fires without a
	// deploy. The startup call has no equivalent — run() reads the process
	// environment and exits — so it is held here, by counting.
	const call = "warnAboutUnroutableProviders("
	if calls := strings.Count(string(source), call) - strings.Count(string(source), "func "+call); calls != 2 {
		t.Errorf("the condition is reported from %d places; it is asked on exactly two paths, startup and reload, and a path that stops asking loses the only signal it has", calls)
	}
}

// TestTheReloadPathActuallyWarns.
//
// TestOneConditionHasOneMessage above counts the message and the condition in
// the source, and both counts stay right when the RELOAD path stops calling
// the function at all: delete the call in reloadSnapshots and the message
// still appears once, `.Lookup(` still appears once, and the whole cmd/kaana
// package still passes. Measured, not reasoned about — that mutation was
// applied and survived, which is what this test exists to answer.
//
// It is the worse half of the condition to lose. Startup runs on a deploy,
// where somebody is already watching; the reload loop is the path that fires
// with no deploy at all, on a snapshot the control plane changed underneath a
// running task, and it is the only warning anyone gets for it.
//
// So this runs the real loop against a real store and reads the real log.
func TestTheReloadPathActuallyWarns(t *testing.T) {
	// An adapter for `openai`, and a snapshot that routes to `cerebras`. The
	// slugs differ on purpose: this is exactly the condition — a published
	// snapshot naming a provider this build cannot reach.
	unroutable := newTestRegistry(t, "openai")
	routable := newTestRegistry(t, "cerebras")

	for _, testCase := range []struct {
		name     string
		adapters *provider.Registry
		warns    bool
	}{
		// The control. Without it a test that never warns for any reason —
		// a loop that never ticks, a store that never reloads — reads as
		// agreement with the case above.
		{name: "the snapshot routes somewhere this build cannot reach", adapters: unroutable, warns: true},
		{name: "the snapshot routes only where it can", adapters: routable, warns: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			logs := &syncBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			store := newTestStore(t, "cerebras", logger)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				reloadSnapshots(ctx, store, rotation.NewRegistry(rotation.Policy{}, nil), testCase.adapters, logger, time.Millisecond)
			}()

			// Wait for the condition rather than for a duration: a fixed sleep
			// is either flaky or slow, and this loop ticks every millisecond.
			deadline := time.After(10 * time.Second)
			for strings.Contains(logs.String(), unroutableMessage) != testCase.warns {
				select {
				case <-deadline:
					t.Fatalf("after 10s the reload log %s the warning; it reads:\n%s",
						map[bool]string{true: "still lacks", false: "carries"}[testCase.warns], logs.String())
				case <-time.After(time.Millisecond):
				}
			}
			cancel()
			<-done
		})
	}
}

// unroutableMessage is the string an alarm filters on, spelled here exactly
// once more than the source does. TestOneConditionHasOneMessage is what keeps
// the source side to one spelling; this is the test side of the same fact.
const unroutableMessage = "the installed snapshot routes to providers this build has no adapter for"

// newTestRegistry builds a registry holding one adapter for one slug. The
// adapter needs no credential — an empty key pool is a legal, `unconfigured`
// adapter, and nothing here ever sends a request.
func newTestRegistry(t *testing.T, slug contract.ProviderSlug) *provider.Registry {
	t.Helper()
	adapter, err := openaicompat.New(openaicompat.Config{
		Provider: slug,
		BaseURL:  "https://kaana-test.invalid/v1",
	})
	if err != nil {
		t.Fatalf("building the %s adapter: %v", slug, err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatalf("registering the %s adapter: %v", slug, err)
	}
	return registry
}

// newTestStore publishes a snapshot naming one deployment of one provider.
func newTestStore(t *testing.T, slug contract.ProviderSlug, logger *slog.Logger) *inventory.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.json")
	document := fmt.Sprintf(`{
		"snapshotId":"snap_test",
		"issuedAt":%q,
		"deployments":[{
			"deploymentId":"dep_test","provider":%q,
			"modelReference":"test/model@2026-05-01","upstreamModelId":"model",
			"regions":["test-region"],"current":true}]}`,
		contract.NewTimestamp(time.Now()), slug)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("writing the inventory: %v", err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("building the inventory store: %v", err)
	}
	return store
}

// syncBuffer is a log sink safe to read while the reload goroutine writes.
type syncBuffer struct {
	mu     sync.Mutex
	buffer strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
