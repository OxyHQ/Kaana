package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type sessionRegistryAdapter struct {
	slug  contract.ProviderSlug
	kinds []contract.RealtimeSessionKind
	pool  *KeyPool
}

func (a sessionRegistryAdapter) Provider() contract.ProviderSlug { return a.slug }
func (a sessionRegistryAdapter) RealtimeSessionKinds() []contract.RealtimeSessionKind {
	return a.kinds
}
func (sessionRegistryAdapter) Open(context.Context, RealtimeOpenRequest, *KeyPool) (RealtimeUpstream, RealtimeOpened, error) {
	return nil, RealtimeOpened{}, nil
}
func (a sessionRegistryAdapter) Health(context.Context) Health { return Health{Provider: a.slug} }
func (a sessionRegistryAdapter) PlatformCredentials() *KeyPool { return a.pool }

// bothKinds claims to execute requests AND hold sessions.
type bothKinds struct {
	registryAdapter
}

func (bothKinds) RealtimeSessionKinds() []contract.RealtimeSessionKind {
	return []contract.RealtimeSessionKind{contract.RealtimeConversation}
}
func (bothKinds) Open(context.Context, RealtimeOpenRequest, *KeyPool) (RealtimeUpstream, RealtimeOpened, error) {
	return nil, RealtimeOpened{}, nil
}

type neitherKind struct{ slug contract.ProviderSlug }

func (a neitherKind) Provider() contract.ProviderSlug { return a.slug }
func (a neitherKind) Health(context.Context) Health   { return Health{Provider: a.slug} }

// TestASlugIsExactlyOneKindOfAdapter is the registration half of the session
// gate: a slug resolves to a request adapter or a session adapter, never
// both, and each resolution refuses the other kind. A text adapter can
// therefore never be handed a realtime deployment.
func TestASlugIsExactlyOneKindOfAdapter(t *testing.T) {
	conversation := []contract.RealtimeSessionKind{contract.RealtimeConversation}
	refused := map[string][]Registrant{
		"both kinds":         {bothKinds{registryAdapter{slug: "both"}}},
		"neither kind":       {neitherKind{slug: "neither"}},
		"no session kind":    {sessionRegistryAdapter{slug: "silent"}},
		"an unknown kind":    {sessionRegistryAdapter{slug: "odd", kinds: []contract.RealtimeSessionKind{"karaoke"}}},
		"one slug, two ways": {registryAdapter{slug: "shared"}, sessionRegistryAdapter{slug: "shared", kinds: conversation}},
	}
	for name, registrants := range refused {
		if _, err := NewRegistry(registrants...); err == nil {
			t.Errorf("%s: registered", name)
		}
	}

	pool, err := NewKeyPool("openai-realtime", []KeyDeclaration{{KeyID: "key_rt", Secret: "sk-realtime-registry-test"}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	textPool, err := NewKeyPool("openai", []KeyDeclaration{{KeyID: "key_text", Secret: "sk-text-registry-test"}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := sessionRegistryAdapter{slug: "openai-realtime", kinds: conversation, pool: pool}
	text := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: "openai"}, pool: textPool}
	registry, err := NewRegistry(session, text)
	if err != nil {
		t.Fatalf("a request adapter and a session adapter under different slugs were refused: %v", err)
	}
	if err := registry.ReplaceGeneration(nil, session, text); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveRealtimeExecution("dep_text", "openai"); !errors.Is(err, ErrNotASessionAdapter) {
		t.Errorf("a session resolved onto the text adapter: %v", err)
	}
	if _, _, err := registry.ResolveExecution("dep_rt", "openai-realtime", true); err == nil {
		t.Error("a request resolved onto the session adapter")
	}
	// Positive controls: each kind resolves where it belongs, with its exact key.
	adapter, view, err := registry.ResolveRealtimeExecution("dep_rt", "openai-realtime")
	if err != nil || adapter.Provider() != "openai-realtime" {
		t.Fatalf("the session adapter did not resolve: %v", err)
	}
	if key, ok := view.Begin().Next(time.Now()); !ok || key.ID != "key_rt" {
		t.Errorf("the session resolved key %q", key.ID)
	}
	if _, _, err := registry.ResolveExecution("dep_text", "openai", true); err != nil {
		t.Errorf("the text adapter did not resolve: %v", err)
	}
	if !registry.Serves("openai-realtime") || !registry.Serves("openai") || registry.Serves("nobody") || len(registry.All()) != 2 {
		t.Error("the registry does not report what it serves")
	}
	if _, err := registry.ResolveCredential("dep_rt", "openai-realtime"); err != nil {
		t.Errorf("the binding gate could not resolve the session deployment: %v", err)
	}
}
