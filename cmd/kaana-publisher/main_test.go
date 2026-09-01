package main

import (
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

func environmentFrom(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestProviderParsingCarriesNoCredential verifies the environment contract is
// adapter configuration only. The store attaches credentials afterwards.
func TestProviderParsingCarriesNoCredential(t *testing.T) {
	providers, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS":                 "cerebras,openrouter",
		"KAANA_PROVIDER_CEREBRAS_REGIONS": "us-east-1,us-west-2",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(providers) != 2 || providers[0].Slug != "cerebras" || providers[1].Slug != "openrouter" {
		t.Fatalf("providers = %+v", slugsOf(providers))
	}
	if providers[0].APIKey != "" || providers[1].APIKey != "" {
		t.Fatal("non-secret provider parsing populated a credential")
	}
	if len(providers[0].Regions) != 2 || providers[0].Regions[0] != "us-east-1" || providers[0].Regions[1] != "us-west-2" {
		t.Errorf("the upstream residency declaration was not carried: %v", providers[0].Regions)
	}
	if providers[1].Regions != nil {
		t.Errorf("an absent upstream residency declaration became %v", providers[1].Regions)
	}
}

func TestProviderRegionDeclarationsAreValidatedAsSets(t *testing.T) {
	for _, value := range []string{"Not Valid", "us-east-1,us-east-1", ","} {
		if _, err := parsePublishableProviders(environmentFrom(map[string]string{
			"KAANA_PROVIDERS":                 "cerebras",
			"KAANA_PROVIDER_CEREBRAS_REGIONS": value,
		})); err == nil {
			t.Errorf("region declaration %q was accepted", value)
		}
	}
}

// TestTheProviderListIsRequired keeps the empty case from meaning "all of them".
func TestTheProviderListIsRequired(t *testing.T) {
	if _, err := parsePublishableProviders(environmentFrom(nil)); err == nil {
		t.Fatal("an absent discovery provider set was accepted")
	}
}

func TestDiscoveryProviderSetCanBeNarrowerThanServing(t *testing.T) {
	providers, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS":           "chutes,ovhcloud,nebius",
		"KAANA_DISCOVERY_PROVIDERS": "nebius",
	}))
	if err != nil {
		t.Fatalf("the serving-only providers poisoned the explicit discovery set: %v", err)
	}
	if len(providers) != 1 || providers[0].Slug != "nebius" {
		t.Fatalf("discovery providers = %+v", slugsOf(providers))
	}
}

func TestDiscoveryProviderSetMustBeAnOrderedSubsetOfServing(t *testing.T) {
	for name, environment := range map[string]map[string]string{
		"missing serving set": {
			"KAANA_DISCOVERY_PROVIDERS": "nebius",
		},
		"provider not served": {
			"KAANA_PROVIDERS":           "openrouter",
			"KAANA_DISCOVERY_PROVIDERS": "openrouter,nebius",
		},
		"serving priority reversed": {
			"KAANA_PROVIDERS":           "cerebras,nebius",
			"KAANA_DISCOVERY_PROVIDERS": "nebius,cerebras",
		},
		"invalid serving slug": {
			"KAANA_PROVIDERS":           "Not Valid,nebius",
			"KAANA_DISCOVERY_PROVIDERS": "nebius",
		},
		"serving prefix collision": {
			"KAANA_PROVIDERS":           "open-router,open.router,nebius",
			"KAANA_DISCOVERY_PROVIDERS": "nebius",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePublishableProviders(environmentFrom(environment)); err == nil {
				t.Fatal("an unservable discovery set was accepted")
			}
		})
	}
}

// TestTwoSlugsFoldingOntoOneVariableAreRefused mirrors the serving process:
// `open-router` and `open.router` are two slugs and one variable name, and the
// loser would silently be asked with the winner's address and credential.
func TestTwoSlugsFoldingOntoOneVariableAreRefused(t *testing.T) {
	_, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS": "open-router,open.router",
	}))
	if err == nil {
		t.Fatal("two slugs folding onto one environment prefix were accepted")
	}
	if !strings.Contains(err.Error(), "KAANA_PROVIDER_OPEN_ROUTER") {
		t.Errorf("the refusal does not name the colliding variable: %v", err)
	}
}

// TestAProviderThatPublishesNoModelListIsRefused: Anthropic answers no
// `GET /models`, and the alternative to refusing is a hand-written list — which
// is the checked-in file this command exists to replace.
func TestAProviderThatPublishesNoModelListIsRefused(t *testing.T) {
	_, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS": "anthropic",
	}))
	if err == nil {
		t.Fatal("a provider with no readable model list was accepted for discovery")
	}
}

// TestAnUnknownSlugNeedsAnAddress: a build that guessed an address would be
// asking somebody nobody chose.
func TestAnUnknownSlugNeedsAnAddress(t *testing.T) {
	if _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS":                          "unknown-provider",
		"KAANA_PROVIDER_UNKNOWN_PROVIDER_PROTOCOL": "openai_compatible",
	})); err == nil {
		t.Fatal("an unknown slug with no base URL was accepted")
	}

	// The control: given the address, the same slug is publishable.
	providers, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS":                          "unknown-provider",
		"KAANA_PROVIDER_UNKNOWN_PROVIDER_PROTOCOL": "openai_compatible",
		"KAANA_PROVIDER_UNKNOWN_PROVIDER_BASE_URL": "https://api.example.invalid/v1",
	}))
	if err != nil {
		t.Fatalf("an unknown slug WITH an address was refused: %v", err)
	}
	if len(providers) != 1 || providers[0].BaseURL != "https://api.example.invalid/v1" {
		t.Errorf("the declared address was not used: %+v", providers)
	}
}

func TestPublisherRefusesACredentialHiddenInTheBaseURL(t *testing.T) {
	_, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS":                          "unknown-provider",
		"KAANA_PROVIDER_UNKNOWN_PROVIDER_PROTOCOL": "openai_compatible",
		"KAANA_PROVIDER_UNKNOWN_PROVIDER_BASE_URL": "https://sk-abcdefghijkl.api.example.invalid/v1",
	}))
	if err == nil {
		t.Fatal("a publisher base URL carrying credential-shaped data was accepted")
	}
}

func TestKnownProviderDiscoveryProfilesAreCarried(t *testing.T) {
	providers, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS": "mistral,siliconflow,nebius,nscale",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if providers[0].Discovery != "mistral_models" || providers[1].Discovery != "siliconflow_models" {
		t.Fatalf("discovery profiles = %q, %q", providers[0].Discovery, providers[1].Discovery)
	}
	if providers[2].Discovery != "nebius_models" || providers[3].Discovery != "openai_models" {
		t.Fatalf("direct-provider discovery profiles = %q, %q", providers[2].Discovery, providers[3].Discovery)
	}
	for _, slug := range []string{"ai21", "chutes", "ovhcloud"} {
		if _, err := parsePublishableProviders(environmentFrom(map[string]string{"KAANA_PROVIDERS": slug})); err == nil {
			t.Fatalf("%s was accepted for an account model-list contract Kaana has not verified", slug)
		}
	}
}

// TestKnownProvidersResolveWithoutAnAddress proves the shared defaults table is
// actually consulted, so `cmd/kaana` and this command reach one address.
func TestKnownProvidersResolveWithoutAnAddress(t *testing.T) {
	providers, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS": "cerebras",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if want := "https://api.cerebras.ai/v1"; providers[0].BaseURL != want {
		t.Errorf("cerebras resolved to %q, want the published root %q", providers[0].BaseURL, want)
	}
	if providers[0].Discovery != "openai_models" {
		t.Errorf("cerebras discovery = %q, want the authenticated OpenAI model-list contract", providers[0].Discovery)
	}
}

// TestOnlyTheFirstKeyOfAPoolIsUsed: the serving process owns rotation. Spending
// a second key on a question whose failure means "ask again later" would retire
// credentials against a call that is not a customer request.
func TestOnlyTheFirstKeyOfAPoolIsUsed(t *testing.T) {
	providers, err := parsePublishableProviders(environmentFrom(map[string]string{
		"KAANA_PROVIDERS": "cerebras",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if err := attachDiscoveryCredentials(providers, map[contract.ProviderSlug][]provider.KeyDeclaration{
		"cerebras": {
			{KeyID: "first", Secret: "first"},
			{KeyID: "second", Secret: "second"},
			{KeyID: "third", Secret: "third"},
		},
	}); err != nil {
		t.Fatalf("attaching credentials: %v", err)
	}
	if providers[0].APIKey != "first" {
		t.Errorf("the publisher took %q from the pool, want the first key", providers[0].APIKey)
	}
}

// TestTheBucketIsNeverDefaulted is the silent-wrong-destination guard. A
// plausible default turns a variable that never arrived into "published
// somewhere else, everything green" — and oxy-api's deploy pipeline has a
// measured way of not delivering a new environment variable at all.
func TestTheBucketIsNeverDefaulted(t *testing.T) {
	if _, err := publisher.NewS3Store(nil, "", "inventory/current.json", "us-west-2",
		publisher.StaticCredentials("id", "secret", "")); err == nil {
		t.Fatal("an empty bucket was accepted; the publish would have gone somewhere nobody chose")
	}
	if _, err := publisher.NewS3Store(nil, "a-bucket", "", "us-west-2",
		publisher.StaticCredentials("id", "secret", "")); err == nil {
		t.Fatal("an empty object key was accepted")
	}
	if _, err := publisher.NewS3Store(nil, "a-bucket", "inventory/current.json", "",
		publisher.StaticCredentials("id", "secret", "")); err == nil {
		t.Fatal("an empty region was accepted")
	}
	// The control: fully specified, it wires.
	if _, err := publisher.NewS3Store(nil, "a-bucket", "inventory/current.json", "us-west-2",
		publisher.StaticCredentials("id", "secret", "")); err != nil {
		t.Fatalf("a fully specified destination was refused: %v", err)
	}
}

// TestAnUnparseableCadenceRefusesRatherThanFallingBack: a typo that silently
// became the default is indistinguishable from the operator's intent.
func TestAnUnparseableCadenceRefusesRatherThanFallingBack(t *testing.T) {
	t.Setenv("KAANA_PUBLISH_INTERVAL", "15minutes")
	if _, err := intervalFromEnv("KAANA_PUBLISH_INTERVAL", publisher.DefaultInterval); err == nil {
		t.Fatal("an unparseable cadence fell back to the default")
	}

	t.Setenv("KAANA_PUBLISH_INTERVAL", "0s")
	if _, err := intervalFromEnv("KAANA_PUBLISH_INTERVAL", publisher.DefaultInterval); err == nil {
		t.Fatal("a zero cadence was accepted")
	}

	t.Setenv("KAANA_PUBLISH_INTERVAL", "5m")
	got, err := intervalFromEnv("KAANA_PUBLISH_INTERVAL", publisher.DefaultInterval)
	if err != nil {
		t.Fatalf("a valid cadence was refused: %v", err)
	}
	if got != 5*time.Minute {
		t.Errorf("cadence = %s, want 5m", got)
	}
}

// TestTheAttributionTableShipsWithTheMeasuredCerebrasModels pins the checked-in
// table to the two ids that were actually read from the provider, so deleting
// the provenance or a line is a failing test rather than a quiet diff.
func TestTheAttributionTableShipsWithTheMeasuredCerebrasModels(t *testing.T) {
	table, err := publisher.LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatalf("loading the shipped attribution table: %v", err)
	}

	for upstream, want := range map[string]contract.ModelID{
		"gpt-oss-120b": "openai/gpt-oss-120b",
		"gemma-4-31b":  "google/gemma-4-31b",
	} {
		got, attributed := table.ModelLine("cerebras", upstream)
		if !attributed {
			t.Errorf("cerebras/%s is no longer attributed; it would be dropped from every snapshot", upstream)
			continue
		}
		if got != want {
			t.Errorf("cerebras/%s attributes to %q, want %q", upstream, got, want)
		}
	}

	// The control: an id nobody measured is NOT attributed, so the assertions
	// above are reading a real table rather than one that answers everything.
	if _, attributed := table.ModelLine("cerebras", "gpt-5-imaginary"); attributed {
		t.Error("the attribution table answers for a model nobody measured")
	}
	if _, attributed := table.ModelLine("openai", "gpt-oss-120b"); attributed {
		t.Error("the attribution table answers for a provider it does not declare")
	}
}
