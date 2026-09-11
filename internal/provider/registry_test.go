package provider

import (
	"context"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type registryAdapter struct{ slug contract.ProviderSlug }

type credentialRegistryAdapter struct {
	registryAdapter
	pool *KeyPool
}

func (a credentialRegistryAdapter) PlatformCredentials() *KeyPool { return a.pool }

type blockingCredentialAdapter struct {
	credentialRegistryAdapter
	entered chan struct{}
	release chan struct{}
}

func (a blockingCredentialAdapter) PlatformCredentials() *KeyPool {
	if a.entered != nil {
		close(a.entered)
		<-a.release
	}
	return a.pool
}

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

func TestExactDeploymentBindingNeverWalksTheProviderPool(t *testing.T) {
	pool, err := NewKeyPool("groq", []KeyDeclaration{{KeyID: "paid", Secret: "paid-secret", Class: KeyClassPaid}, {KeyID: "free", Secret: "free-secret", Class: KeyClassFree}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "groq"}, pool: pool}
	registry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceGeneration([]CredentialBinding{{DeploymentID: "dep_free", Provider: "groq", KeyID: "free"}, {DeploymentID: "dep_paid", Provider: "groq", KeyID: "paid"}}, adapter); err != nil {
		t.Fatal(err)
	}
	_, bound, err := registry.ResolveExecution("dep_paid", "groq", true)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := bound.Begin().Next(time.Now())
	if !ok || key.ID != "paid" {
		t.Fatalf("selected key %q, ok=%v", key.ID, ok)
	}
	if _, _, err := registry.ResolveExecution("dep_missing", "groq", true); err == nil {
		t.Fatal("missing binding fell back to pool")
	}
	if _, _, err := registry.ResolveExecution("dep_paid", "openai", true); err == nil {
		t.Fatal("provider mismatch was accepted")
	}
}

func TestResolveExecutionCannotMixReloadGenerations(t *testing.T) {
	oldPool, _ := NewKeyPool("groq", []KeyDeclaration{{KeyID: "old", Secret: "old-secret"}}, KeyPolicy{}, nil)
	newPool, _ := NewKeyPool("groq", []KeyDeclaration{{KeyID: "new", Secret: "new-secret"}}, KeyPolicy{}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	oldAdapter := &blockingCredentialAdapter{credentialRegistryAdapter: credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "groq"}, pool: oldPool}}
	newAdapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "groq"}, pool: newPool}
	registry, _ := NewRegistry(oldAdapter)
	if err := registry.ReplaceGeneration([]CredentialBinding{{DeploymentID: "dep", Provider: "groq", KeyID: "old"}}, oldAdapter); err != nil {
		t.Fatal(err)
	}
	oldAdapter.entered, oldAdapter.release = entered, release
	type result struct {
		adapter Adapter
		pool    *KeyPool
		err     error
	}
	resolved := make(chan result, 1)
	go func() {
		adapter, pool, err := registry.ResolveExecution("dep", "groq", true)
		resolved <- result{adapter, pool, err}
	}()
	<-entered
	replaced := make(chan error, 1)
	go func() {
		replaced <- registry.ReplaceGeneration([]CredentialBinding{{DeploymentID: "dep", Provider: "groq", KeyID: "new"}}, newAdapter)
	}()
	select {
	case <-replaced:
		t.Fatal("reload crossed an in-flight generation read")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	got := <-resolved
	if got.err != nil || got.adapter != oldAdapter {
		t.Fatalf("resolved mixed adapter: %T %v", got.adapter, got.err)
	}
	key, ok := got.pool.Begin().Next(time.Now())
	if !ok || key.ID != "old" {
		t.Fatalf("resolved mixed key %q", key.ID)
	}
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}
}
