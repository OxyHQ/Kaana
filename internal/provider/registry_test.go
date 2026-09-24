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

func TestAnUnboundDeploymentUsesItsProvidersOnlyKey(t *testing.T) {
	pool, err := NewKeyPool("xai", []KeyDeclaration{{KeyID: "only", Secret: "only-secret"}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "xai"}, pool: pool}
	registry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceGeneration(nil, adapter); err != nil {
		t.Fatal(err)
	}
	_, view, err := registry.ResolveExecution("dep_xai_tts_new", "xai", true)
	if err != nil {
		t.Fatalf("a single-key provider left its new deployment unroutable: %v", err)
	}
	key, ok := view.Begin().Next(time.Now())
	if !ok || key.ID != "only" {
		t.Fatalf("provider default selected %q, ok=%v", key.ID, ok)
	}
	// A BYOK route never reads the platform key, default or not.
	if _, view, err := registry.ResolveExecution("dep_xai_tts_new", "xai", false); err != nil || view != nil {
		t.Fatalf("a customer route resolved a platform credential: %v, %v", view, err)
	}
}

func TestAnUnboundDeploymentOfAMultiKeyProviderIsUnroutable(t *testing.T) {
	pool, err := NewKeyPool("openrouter", []KeyDeclaration{{KeyID: "a", Secret: "a-secret"}, {KeyID: "b", Secret: "b-secret"}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "openrouter"}, pool: pool}
	registry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceGeneration([]CredentialBinding{{DeploymentID: "dep_bound", Provider: "openrouter", KeyID: "b"}}, adapter); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveExecution("dep_new", "openrouter", true); err == nil {
		t.Fatal("an unbound deployment of a two-key provider was given one of its keys")
	}
	// The control: its bound sibling resolves, to exactly its key.
	_, view, err := registry.ResolveExecution("dep_bound", "openrouter", true)
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := view.Begin().Next(time.Now()); !ok || key.ID != "b" {
		t.Fatalf("bound deployment selected %q, ok=%v", key.ID, ok)
	}
}

// TestAnExactBindingIsNeverEscapedForTheProviderDefault: the provider default
// applies only where NO exact binding exists. A deployment bound to a retired
// key is not given its provider's other key, and one whose binding names
// another provider does not fall through to this provider's only key.
func TestAnExactBindingIsNeverEscapedForTheProviderDefault(t *testing.T) {
	now := time.Now()
	pool, err := NewKeyPool("groq", []KeyDeclaration{
		{KeyID: "retired", Secret: "retired-secret", State: CredentialRuntimeState{Reason: KeyExhausted, RetiredUntil: now.Add(time.Hour)}},
		{KeyID: "healthy", Secret: "healthy-secret"},
	}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "groq"}, pool: pool}
	registry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceGeneration([]CredentialBinding{{DeploymentID: "dep_exact", Provider: "groq", KeyID: "retired"}}, adapter); err != nil {
		t.Fatal(err)
	}
	_, view, err := registry.ResolveExecution("dep_exact", "groq", true)
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := view.Begin().Next(now); ok {
		t.Fatalf("a deployment bound to a retired key was served by %q", key.ID)
	}

	// With one key, the retired exact key is also the provider's only key, and
	// still yields nothing: the default is a key, not a fallback.
	single, err := NewKeyPool("groq", []KeyDeclaration{
		{KeyID: "retired", Secret: "retired-secret", State: CredentialRuntimeState{Reason: KeyExhausted, RetiredUntil: now.Add(time.Hour)}},
	}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	singleAdapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "groq"}, pool: single}
	if err := registry.ReplaceGeneration(nil, singleAdapter); err != nil {
		t.Fatal(err)
	}
	_, view, err = registry.ResolveExecution("dep_new", "groq", true)
	if err != nil {
		t.Fatalf("a retired single key stopped being its provider's default: %v", err)
	}
	if key, ok := view.Begin().Next(now); ok {
		t.Fatalf("a retired provider-default key served %q", key.ID)
	}

	// An exact binding naming another provider is final too.
	other, err := NewKeyPool("xai", []KeyDeclaration{{KeyID: "xai-only", Secret: "xai-secret"}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	xai := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "xai"}, pool: other}
	if err := registry.ReplaceGeneration([]CredentialBinding{{DeploymentID: "dep_groq", Provider: "groq", KeyID: "retired"}}, singleAdapter, xai); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveExecution("dep_groq", "xai", true); err == nil {
		t.Fatal("a deployment bound under another provider fell through to this provider's only key")
	}
}
