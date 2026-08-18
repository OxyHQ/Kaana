package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/provider"
)

// TestFailoverAcknowledgementCannotBeSetByAccident.
//
// Same-model failover overrides a routing-policy control the envelope does not
// carry, so enabling it is a statement someone makes on purpose. The parser is
// what makes that true: an empty variable, a bare "true", a "yes", or a reason
// with no date all leave the safe default in place or refuse to start, and none
// of them turns it on.
func TestFailoverAcknowledgementCannotBeSetByAccident(t *testing.T) {
	refusedOrOff := []struct {
		name  string
		value string
		fails bool
	}{
		{name: "unset", value: "", fails: false},
		{name: "whitespace", value: "   ", fails: false},
		{name: "a bare truthy word", value: "true", fails: true},
		{name: "an enthusiastic yes", value: "yes", fails: true},
		{name: "a reason with no date", value: "alia-canary:", fails: true},
		{name: "a date with no reason", value: ":2026-08-16", fails: true},
		{name: "a date nobody can parse", value: "alia-canary:soon", fails: true},
		{name: "a date in the wrong order", value: "alia-canary:16-08-2026", fails: true},
	}

	for _, testCase := range refusedOrOff {
		t.Run(testCase.name, func(t *testing.T) {
			acknowledgement, err := failoverAcknowledgement(testCase.value)
			if testCase.fails && err == nil {
				t.Fatalf("%q was accepted", testCase.value)
			}
			if !testCase.fails && err != nil {
				t.Fatalf("%q was refused: %v", testCase.value, err)
			}
			if acknowledgement != "" {
				t.Errorf("%q enabled failover", testCase.value)
			}
		})
	}

	// The control: a well-formed acknowledgement does enable it, or every case
	// above would pass on a parser that refuses everything.
	acknowledgement, err := failoverAcknowledgement(" alia-first-party-canary:2026-08-16 ")
	if err != nil {
		t.Fatalf("a well-formed acknowledgement was refused: %v", err)
	}
	if !strings.Contains(acknowledgement, "alia-first-party-canary") {
		t.Errorf("the acknowledgement is recorded as %q; it names who accepted it", acknowledgement)
	}
}

// TestAProviderSlugResolvesToItsOwnAdapterAddressAndCredentials.
//
// The inventory names a provider SLUG and holds nothing else about it — no
// address, and above all no credential, which would be a copy of an Oxy entity.
// So the slug is resolved here, and what a reviewer needs to see is that three
// providers speaking ONE protocol are three configurations rather than three
// adapters.
func TestAProviderSlugResolvesToItsOwnAdapterAddressAndCredentials(t *testing.T) {
	environment := map[string]string{
		"RELAY_PROVIDERS":                                     "openai,openrouter,cerebras,anthropic",
		"RELAY_PROVIDER_OPENAI_API_KEY":                       "fake-openai-key",
		"RELAY_PROVIDER_OPENROUTER_API_KEY":                   " fake-openrouter-a , fake-openrouter-b ,",
		"RELAY_PROVIDER_OPENROUTER_KEYS_ON_SEPARATE_ACCOUNTS": "true",
		"RELAY_PROVIDER_OPENROUTER_KEY_RETIREMENT":            "45m",
		"RELAY_PROVIDER_CEREBRAS_BASE_URL":                    "https://cerebras.example.invalid/v1",
		"RELAY_PROVIDER_ANTHROPIC_API_KEY":                    "fake-anthropic-key",
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
		if got := byName[slug].Protocol; got != protocolOpenAICompatible {
			t.Errorf("%s resolves to protocol %q, expected %q", slug, got, protocolOpenAICompatible)
		}
	}
	if byName["anthropic"].Protocol != protocolAnthropicMessages {
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

	// A pool of two, from one variable, with the empty entry a trailing
	// separator leaves behind discarded rather than sent as a credential.
	openrouter := byName["openrouter"]
	if len(openrouter.APIKeys) != 2 {
		t.Fatalf("openrouter declares %d credentials, expected 2 (%v)", len(openrouter.APIKeys), openrouter.APIKeys)
	}
	if openrouter.APIKeys[0] != "fake-openrouter-a" || openrouter.APIKeys[1] != "fake-openrouter-b" {
		t.Errorf("the credentials were not trimmed to their declared order: %v", openrouter.APIKeys)
	}
	if !openrouter.Keys.OnSeparateAccounts {
		t.Error("the operator declared openrouter's keys are separate accounts and the policy does not carry it")
	}
	if openrouter.Keys.Retirement != 45*time.Minute {
		t.Errorf("openrouter's retirement window is %s, expected 45m", openrouter.Keys.Retirement)
	}
	// A provider with no credential is a supported state, and is not the same
	// as one that was never declared: it reports itself unconfigured on the
	// health surface rather than failing at the first request.
	if len(byName["cerebras"].APIKeys) != 0 {
		t.Error("cerebras declares no credential and one appeared")
	}

	// Every configuration above must actually build an adapter reporting the
	// slug it was configured under. A registry keys on the adapter's OWN slug,
	// so a mis-resolution here would surface as a duplicate or as a receipt
	// attributed to the wrong provider.
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
		"no providers at all": {
			"RELAY_PROVIDER_OPENAI_API_KEY": "fake-openai-key",
		},
		"a slug that is not a slug": {
			"RELAY_PROVIDERS": "Open AI",
		},
		"the same provider twice": {
			"RELAY_PROVIDERS": "openai,openai",
		},
		// `open-router` and `open.router` are two slugs and one variable name.
		// Left alone, the second would silently be configured with the first
		// one's address and credentials.
		"two slugs reading one variable": {
			"RELAY_PROVIDERS":                     "open-router,open.router",
			"RELAY_PROVIDER_OPEN_ROUTER_BASE_URL": "https://openrouter.example.invalid/v1",
			"RELAY_PROVIDER_OPEN_ROUTER_PROTOCOL": protocolOpenAICompatible,
		},
		"a provider this build knows no protocol for": {
			"RELAY_PROVIDERS":              "groq",
			"RELAY_PROVIDER_GROQ_BASE_URL": "https://groq.example.invalid/v1",
		},
		"a protocol this build does not speak": {
			"RELAY_PROVIDERS":              "groq",
			"RELAY_PROVIDER_GROQ_PROTOCOL": "cohere_v1",
			"RELAY_PROVIDER_GROQ_BASE_URL": "https://groq.example.invalid/v1",
		},
		"a provider this build knows no address for": {
			"RELAY_PROVIDERS":              "groq",
			"RELAY_PROVIDER_GROQ_PROTOCOL": protocolOpenAICompatible,
		},
		// The Messages API adapter reports its slug as a constant, so serving
		// it under another name would attribute every event and every usage
		// record to `anthropic` while the inventory routed elsewhere.
		"the messages api under another provider's name": {
			"RELAY_PROVIDERS":                 "bedrock",
			"RELAY_PROVIDER_BEDROCK_PROTOCOL": protocolAnthropicMessages,
			"RELAY_PROVIDER_BEDROCK_BASE_URL": "https://bedrock.example.invalid/v1",
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
		"RELAY_PROVIDERS":              "groq",
		"RELAY_PROVIDER_GROQ_PROTOCOL": protocolOpenAICompatible,
		"RELAY_PROVIDER_GROQ_BASE_URL": "https://groq.example.invalid/v1",
	}
	if _, err := parseProviders(lookup(accepted)); err != nil {
		t.Errorf("a well-formed provider declaration was refused: %v", err)
	}
}

// TestADuplicatedCredentialRefusesToStart.
//
// Two entries holding one credential look like a pool of two, exhaust as one,
// and halve the pool the first time it is spent. It is refused where it is
// read, and the refusal names positions rather than quoting the secret it is
// complaining about — a startup error is a log line.
func TestADuplicatedCredentialRefusesToStart(t *testing.T) {
	configs, err := parseProviders(lookup(map[string]string{
		"RELAY_PROVIDERS":               "openai",
		"RELAY_PROVIDER_OPENAI_API_KEY": "fake-openai-key,fake-openai-key",
	}))
	if err != nil {
		t.Fatalf("the configuration was refused before the adapter was built: %v", err)
	}
	_, err = buildAdapters(configs)
	if err == nil {
		t.Fatal("one credential declared twice was accepted as a pool of two")
	}
	if strings.Contains(err.Error(), "fake-openai-key") {
		t.Errorf("the refusal quotes the credential: %v", err)
	}
}

// lookup turns a map into the getenv parseProviders reads, so the whole of it
// is testable without a process environment.
func lookup(environment map[string]string) func(string) string {
	return func(name string) string { return environment[name] }
}

// TestPerProviderHeadersAreConfigurableAndNeverCarryACredential.
//
// OpenRouter is served by an adapter whose `Headers` field exists for its
// attribution headers, and a provider wired without them is compliant until it
// is not — which is worse than being wired wrong, because nothing fails until
// the provider decides it should.
func TestPerProviderHeadersAreConfigurableAndNeverCarryACredential(t *testing.T) {
	configs, err := parseProviders(lookup(map[string]string{
		"RELAY_PROVIDERS":                   "openrouter",
		"RELAY_PROVIDER_OPENROUTER_HEADERS": "HTTP-Referer=https://oxy.so, X-Title=Oxy ",
	}))
	if err != nil {
		t.Fatalf("the headers were refused: %v", err)
	}
	headers := configs[0].Headers
	// `=` is the separator precisely so a value can be a URL, which is what an
	// attribution header holds.
	if headers["HTTP-Referer"] != "https://oxy.so" || headers["X-Title"] != "Oxy" {
		t.Errorf("the headers parsed as %v", headers)
	}

	for name, environment := range map[string]map[string]string{
		"a pair with no value": {
			"RELAY_PROVIDERS": "openrouter", "RELAY_PROVIDER_OPENROUTER_HEADERS": "HTTP-Referer",
		},
		"a value with no name": {
			"RELAY_PROVIDERS": "openrouter", "RELAY_PROVIDER_OPENROUTER_HEADERS": "=Oxy",
		},
		"the same header twice": {
			"RELAY_PROVIDERS": "openrouter", "RELAY_PROVIDER_OPENROUTER_HEADERS": "X-Title=Oxy,X-Title=Other",
		},
		// A credential here would look configured, be overwritten by the
		// adapter at send time, and sit in a plain variable outside the pool
		// that exists to manage it.
		"an authorization header": {
			"RELAY_PROVIDERS": "openrouter", "RELAY_PROVIDER_OPENROUTER_HEADERS": "Authorization=Bearer fake-key",
		},
		"a provider key header": {
			"RELAY_PROVIDERS": "openrouter", "RELAY_PROVIDER_OPENROUTER_HEADERS": "x-api-key=fake-key",
		},
		// The Messages API adapter carries no extra headers, so accepting them
		// would drop them silently — the same outcome as never setting them and
		// harder to see.
		"headers for a protocol that sends none": {
			"RELAY_PROVIDERS": "anthropic", "RELAY_PROVIDER_ANTHROPIC_HEADERS": "X-Title=Oxy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProviders(lookup(environment)); err == nil {
				t.Error("the configuration was accepted")
			}
		})
	}
}

// TestACredentialForAProviderNobodyServesIsNamed.
//
// It is the one failure in this area with no downstream signal: the task
// starts, the health probe passes, the rollout reports complete, and the
// provider is simply absent. Every check the infrastructure can run says green,
// because from outside the process there is nothing wrong with it.
func TestACredentialForAProviderNobodyServesIsNamed(t *testing.T) {
	configs, err := parseProviders(lookup(map[string]string{
		"RELAY_PROVIDERS":                     "cerebras,open-router",
		"RELAY_PROVIDER_CEREBRAS_BASE_URL":    "https://cerebras.example.invalid/v1",
		"RELAY_PROVIDER_CEREBRAS_PROTOCOL":    protocolOpenAICompatible,
		"RELAY_PROVIDER_OPEN_ROUTER_BASE_URL": "https://openrouter.example.invalid/v1",
		"RELAY_PROVIDER_OPEN_ROUTER_PROTOCOL": protocolOpenAICompatible,
	}))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}

	environment := []string{
		"RELAY_PROVIDER_CEREBRAS_API_KEY=fake-cerebras-key",
		// The slug is `open-router`, and its variables fold the hyphen to an
		// underscore. A check that compared slugs rather than prefixes would
		// report this one as unused.
		"RELAY_PROVIDER_OPEN_ROUTER_API_KEY=fake-openrouter-key",
		"RELAY_PROVIDER_OPENAI_API_KEY=fake-openai-key",
		// Neither of these is a provider credential, and a pattern that matched
		// on the prefix alone would name both.
		"RELAY_PROVIDER_RATES_PATH=/etc/relay/rates.json",
		"RELAY_PROVIDER_CEREBRAS_BASE_URL=https://cerebras.example.invalid/v1",
		"PATH=/usr/bin",
	}

	unused := unusedProviderCredentials(environment, configs)
	if len(unused) != 1 || unused[0] != "RELAY_PROVIDER_OPENAI_API_KEY" {
		t.Fatalf("named %v as unused; exactly the openai key is", unused)
	}

	// The control: with openai declared too, nothing is unused — or a check
	// that named everything would pass the assertion above by accident.
	served, err := parseProviders(lookup(map[string]string{
		"RELAY_PROVIDERS":                     "cerebras,open-router,openai",
		"RELAY_PROVIDER_CEREBRAS_BASE_URL":    "https://cerebras.example.invalid/v1",
		"RELAY_PROVIDER_CEREBRAS_PROTOCOL":    protocolOpenAICompatible,
		"RELAY_PROVIDER_OPEN_ROUTER_BASE_URL": "https://openrouter.example.invalid/v1",
		"RELAY_PROVIDER_OPEN_ROUTER_PROTOCOL": protocolOpenAICompatible,
	}))
	if err != nil {
		t.Fatalf("the control configuration was refused: %v", err)
	}
	if unused := unusedProviderCredentials(environment, served); len(unused) != 0 {
		t.Errorf("named %v as unused when every provider is served", unused)
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
			"RELAY_PROVIDERS": "openai",
			"RELAY_PROVIDER_OPENAI_KEYS_ON_SEPARATE_ACCOUNTS": spelling,
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
	configs, err := parseProviders(lookup(map[string]string{"RELAY_PROVIDERS": "openai"}))
	if err != nil {
		t.Fatalf("an absent declaration was refused: %v", err)
	}
	if configs[0].Keys.OnSeparateAccounts {
		t.Fatal("an unset variable declared separate accounts")
	}
	for _, spelling := range []string{"false", "FALSE", " false "} {
		configs, err := parseProviders(lookup(map[string]string{
			"RELAY_PROVIDERS": "openai",
			"RELAY_PROVIDER_OPENAI_KEYS_ON_SEPARATE_ACCOUNTS": spelling,
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
			"RELAY_PROVIDERS": "openai",
			"RELAY_PROVIDER_OPENAI_KEYS_ON_SEPARATE_ACCOUNTS": spelling,
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
}
