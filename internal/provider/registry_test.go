package provider

import (
	"context"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type registryAdapter struct{ slug contract.ProviderSlug }

func (a registryAdapter) Provider() contract.ProviderSlug { return a.slug }
func (registryAdapter) Translate(*contract.Request, Route) (*Call, error) {
	return nil, nil
}
func (registryAdapter) Stream(context.Context, *Call, Emitter, *KeyPool) (Outcome, error) {
	return Outcome{}, nil
}
func (a registryAdapter) Health(context.Context) Health { return Health{Provider: a.slug} }

func TestRegistryReplacementIsValidatedBeforeItBecomesVisible(t *testing.T) {
	registry, err := NewRegistry(registryAdapter{slug: "groq"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Replace(registryAdapter{slug: "mistral"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, found := registry.Lookup("groq"); found {
		t.Fatal("old adapter remained visible after replacement")
	}
	if _, found := registry.Lookup("mistral"); !found {
		t.Fatal("new adapter was not visible after replacement")
	}
	if err := registry.Replace(registryAdapter{slug: "mistral"}, registryAdapter{slug: "mistral"}); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	if _, found := registry.Lookup("mistral"); !found {
		t.Fatal("a rejected replacement changed the live registry")
	}
}
