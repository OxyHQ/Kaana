package publisher_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

func TestMistralDiscoveryKeepsOnlyChatCompletionModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"mistral-small-2603","capabilities":{"completion_chat":true}},{"id":"mistral-embed","capabilities":{"completion_chat":false}}]}`))
	}))
	t.Cleanup(server.Close)

	models, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{
		Slug: "mistral", BaseURL: server.URL + "/v1", APIKey: "test-key", Discovery: providerconfig.DiscoveryMistralModels,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 1 || models[0].UpstreamModelID != "mistral-small-2603" {
		t.Fatalf("models = %+v", models)
	}
}

func TestSiliconFlowDiscoveryRequestsOnlyTextChatModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "text" || r.URL.Query().Get("sub_type") != "chat" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3.5-397B-A17B"}]}`))
	}))
	t.Cleanup(server.Close)

	models, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{
		Slug: "siliconflow", BaseURL: server.URL + "/v1", APIKey: "test-key", Discovery: providerconfig.DiscoverySiliconModels,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 1 || models[0].UpstreamModelID != "Qwen/Qwen3.5-397B-A17B" {
		t.Fatalf("models = %+v", models)
	}
}

func TestNebiusDiscoveryRejectsDeliveryFlavours(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("verbose"); got != "true" {
			t.Errorf("verbose = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"meta-llama/Meta-Llama-3.1-70B-Instruct"},
			{"id":"meta-llama/Meta-Llama-3.1-70B-Instruct-fast"}
		]}`))
	}))
	t.Cleanup(server.Close)

	models, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{
		Slug: "nebius", BaseURL: server.URL + "/v1", APIKey: "test-key", Discovery: providerconfig.DiscoveryNebiusModels,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 1 || models[0].UpstreamModelID != "meta-llama/Meta-Llama-3.1-70B-Instruct" {
		t.Fatalf("models = %+v", models)
	}
}
