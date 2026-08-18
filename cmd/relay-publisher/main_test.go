package main

import (
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/publisher"
)

func environmentFrom(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestAProviderWithoutACredentialIsSkippedRatherThanDeclared is the first of
// the three constraints the README states on whoever publishes the inventory: a
// snapshot may not name a provider whose key does not exist, because no value
// of RELAY_PROVIDERS serves it without either refusing its references or
// pinning a permanent `unconfigured` alarm.
func TestAProviderWithoutACredentialIsSkippedRatherThanDeclared(t *testing.T) {
	providers, skipped, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":                 "cerebras,openrouter",
		"RELAY_PROVIDER_CEREBRAS_API_KEY": "a-key",
		// openrouter is declared with no key.
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(providers) != 1 || providers[0].Slug != "cerebras" {
		t.Fatalf("expected only cerebras to be publishable, got %+v", slugsOf(providers))
	}
	if len(skipped) != 1 || skipped[0] != "openrouter" {
		t.Errorf("expected openrouter to be reported as skipped, got %v", skipped)
	}
}

// TestNoCredentialAtAllRefuses: publishing a snapshot that names nothing would
// be refused by Relay's own parse, so it is refused here where the operator can
// still read why.
func TestNoCredentialAtAllRefuses(t *testing.T) {
	if _, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS": "cerebras,openrouter",
	})); err == nil {
		t.Fatal("a run in which no provider holds a credential was accepted")
	}
}

// TestTheProviderListIsRequired keeps the empty case from meaning "all of them".
func TestTheProviderListIsRequired(t *testing.T) {
	if _, _, err := parsePublishableProviders(environmentFrom(nil)); err == nil {
		t.Fatal("an absent RELAY_PROVIDERS was accepted")
	}
}

// TestTwoSlugsFoldingOntoOneVariableAreRefused mirrors the serving process:
// `open-router` and `open.router` are two slugs and one variable name, and the
// loser would silently be asked with the winner's address and credential.
func TestTwoSlugsFoldingOntoOneVariableAreRefused(t *testing.T) {
	_, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":                    "open-router,open.router",
		"RELAY_PROVIDER_OPEN_ROUTER_API_KEY": "a-key",
	}))
	if err == nil {
		t.Fatal("two slugs folding onto one environment prefix were accepted")
	}
	if !strings.Contains(err.Error(), "RELAY_PROVIDER_OPEN_ROUTER") {
		t.Errorf("the refusal does not name the colliding variable: %v", err)
	}
}

// TestAProviderThatPublishesNoModelListIsRefused: Anthropic answers no
// `GET /models`, and the alternative to refusing is a hand-written list — which
// is the checked-in file this command exists to replace.
func TestAProviderThatPublishesNoModelListIsRefused(t *testing.T) {
	_, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":                  "anthropic",
		"RELAY_PROVIDER_ANTHROPIC_API_KEY": "a-key",
	}))
	if err == nil {
		t.Fatal("a provider with no readable model list was accepted for discovery")
	}
}

// TestAnUnknownSlugNeedsAnAddress: a build that guessed an address would be
// asking somebody nobody chose.
func TestAnUnknownSlugNeedsAnAddress(t *testing.T) {
	if _, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":              "groq",
		"RELAY_PROVIDER_GROQ_API_KEY":  "a-key",
		"RELAY_PROVIDER_GROQ_PROTOCOL": "openai_compatible",
	})); err == nil {
		t.Fatal("an unknown slug with no base URL was accepted")
	}

	// The control: given the address, the same slug is publishable.
	providers, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":              "groq",
		"RELAY_PROVIDER_GROQ_API_KEY":  "a-key",
		"RELAY_PROVIDER_GROQ_PROTOCOL": "openai_compatible",
		"RELAY_PROVIDER_GROQ_BASE_URL": "https://api.groq.com/openai/v1",
	}))
	if err != nil {
		t.Fatalf("an unknown slug WITH an address was refused: %v", err)
	}
	if len(providers) != 1 || providers[0].BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("the declared address was not used: %+v", providers)
	}
}

// TestKnownProvidersResolveWithoutAnAddress proves the shared defaults table is
// actually consulted, so `cmd/relay` and this command reach one address.
func TestKnownProvidersResolveWithoutAnAddress(t *testing.T) {
	providers, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":                 "cerebras",
		"RELAY_PROVIDER_CEREBRAS_API_KEY": "a-key",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if want := "https://api.cerebras.ai/v1"; providers[0].BaseURL != want {
		t.Errorf("cerebras resolved to %q, want the published root %q", providers[0].BaseURL, want)
	}
}

// TestOnlyTheFirstKeyOfAPoolIsUsed: the serving process owns rotation. Spending
// a second key on a question whose failure means "ask again later" would retire
// credentials against a call that is not a customer request.
func TestOnlyTheFirstKeyOfAPoolIsUsed(t *testing.T) {
	providers, _, err := parsePublishableProviders(environmentFrom(map[string]string{
		"RELAY_PROVIDERS":                 "cerebras",
		"RELAY_PROVIDER_CEREBRAS_API_KEY": "first,second,third",
	}))
	if err != nil {
		t.Fatalf("parsing: %v", err)
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
	t.Setenv("RELAY_PUBLISH_INTERVAL", "15minutes")
	if _, err := intervalFromEnv("RELAY_PUBLISH_INTERVAL", publisher.DefaultInterval); err == nil {
		t.Fatal("an unparseable cadence fell back to the default")
	}

	t.Setenv("RELAY_PUBLISH_INTERVAL", "0s")
	if _, err := intervalFromEnv("RELAY_PUBLISH_INTERVAL", publisher.DefaultInterval); err == nil {
		t.Fatal("a zero cadence was accepted")
	}

	t.Setenv("RELAY_PUBLISH_INTERVAL", "5m")
	got, err := intervalFromEnv("RELAY_PUBLISH_INTERVAL", publisher.DefaultInterval)
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
