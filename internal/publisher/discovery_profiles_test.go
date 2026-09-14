package publisher_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func TestSiliconFlowDiscoveryRequestsTextModelsIncludingEmbeddings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "text" || r.URL.Query().Has("sub_type") {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3.5-397B-A17B"},{"id":"Qwen/Qwen3-Embedding-0.6B"}]}`))
	}))
	t.Cleanup(server.Close)

	models, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{
		Slug: "siliconflow", BaseURL: server.URL + "/v1", APIKey: "test-key", Discovery: providerconfig.DiscoverySiliconModels,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 2 || models[0].UpstreamModelID != "Qwen/Qwen3-Embedding-0.6B" {
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

func TestAlibabaDiscoveryUsesTheNativePaginatedTextCatalogue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.URL.Query()["capabilities"]; !reflect.DeepEqual(got, []string{"TG"}) {
			t.Errorf("capabilities = %v", got)
		}
		if got := r.URL.Query()["supports"]; !reflect.DeepEqual(got, []string{"inference"}) {
			t.Errorf("supports = %v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page_no") {
		case "1":
			_, _ = w.Write([]byte(`{
				"success":true,
				"output":{"total":3,"page_no":1,"page_size":100,"models":[
					{"model":"qwen3.7-plus-2026-05-26","inference_metadata":{"response_modality":["Text"]}},
					{"model":"qwen-image-2.0-pro-2026-06-22","inference_metadata":{"response_modality":["Image"]}}
				]}
			}`))
		case "2":
			_, _ = w.Write([]byte(`{
				"success":true,
				"output":{"total":3,"page_no":2,"page_size":100,"models":[
					{"model":"qwen3.6-flash-2026-04-16","inference_metadata":{"response_modality":["Text"]}}
				]}
			}`))
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	models, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{
		Slug: "alibaba", BaseURL: server.URL + "/compatible-mode/v1", APIKey: "test-key", Discovery: providerconfig.DiscoveryAlibabaModels,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []publisher.DiscoveredModel{
		{UpstreamModelID: "qwen3.6-flash-2026-04-16"},
		{UpstreamModelID: "qwen3.7-plus-2026-05-26"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
}

func TestAlibabaDiscoveryRefusesInconsistentPagination(t *testing.T) {
	for name, secondPage := range map[string]string{
		"changed total":        `{"success":true,"output":{"total":3,"page_no":2,"page_size":100,"models":[{"model":"qwen-b","inference_metadata":{"response_modality":["Text"]}}]}}`,
		"duplicate id":         `{"success":true,"output":{"total":2,"page_no":2,"page_size":100,"models":[{"model":"qwen-a","inference_metadata":{"response_modality":["Text"]}}]}}`,
		"unexpected page size": `{"success":true,"output":{"total":2,"page_no":2,"page_size":50,"models":[{"model":"qwen-b","inference_metadata":{"response_modality":["Text"]}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("page_no") == "1" {
					_, _ = w.Write([]byte(`{"success":true,"output":{"total":2,"page_no":1,"page_size":100,"models":[{"model":"qwen-a","inference_metadata":{"response_modality":["Text"]}}]}}`))
					return
				}
				_, _ = fmt.Fprint(w, secondPage)
			}))
			t.Cleanup(server.Close)

			_, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{
				Slug: "alibaba", BaseURL: server.URL + "/compatible-mode/v1", APIKey: "test-key", Discovery: providerconfig.DiscoveryAlibabaModels,
			})
			if err == nil {
				t.Fatal("inconsistent pagination was accepted")
			}
		})
	}
}

func TestAlibabaDiscoveryUsesTheDocumentedRegionalCatalogueOrigin(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.URL.String(); got != "https://dashscope-intl.aliyuncs.com/api/v1/models?capabilities=TG&page_no=1&page_size=100&supports=inference" {
			t.Errorf("catalogue URL = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"success":true,"output":{"total":1,"page_no":1,"page_size":100,"models":[{"model":"qwen3.7-plus-2026-05-26","inference_metadata":{"response_modality":["Text"]}}]}}`,
			)),
			Request: request,
		}, nil
	})}
	models, err := publisher.Discover(context.Background(), client, publisher.Provider{
		Slug: "alibaba", BaseURL: "https://workspace-opaque.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
		APIKey: "test-key", Discovery: providerconfig.DiscoveryAlibabaModels,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 1 || models[0].UpstreamModelID != "qwen3.7-plus-2026-05-26" {
		t.Fatalf("models = %+v", models)
	}
}

func TestAlibabaDiscoveryRefusesAnOriginThatDoesNotIdentifyItsCatalogueWorkspace(t *testing.T) {
	for _, baseURL := range []string{
		"https://dashscope.aliyuncs.com/compatible-mode/v1",
		"https://dashscope-us.aliyuncs.com/compatible-mode/v1",
	} {
		_, err := publisher.Discover(context.Background(), http.DefaultClient, publisher.Provider{
			Slug: "alibaba", BaseURL: baseURL, APIKey: "test-key", Discovery: providerconfig.DiscoveryAlibabaModels,
		})
		if err == nil {
			t.Errorf("non-workspace serving origin %q was accepted as a catalogue origin", baseURL)
		}
	}
}

type discoveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (function discoveryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestXAISpeechDiscoveryRequiresTheAuthenticatedVoiceCatalogue(t *testing.T) {
	for _, tc := range []struct {
		name, voices string
		wantSpeech   bool
	}{
		{"both voices", `{"voices":[{"voice_id":"eve"},{"voice_id":"rex"}]}`, true},
		{"no voices", `{"voices":[]}`, false},
		{"missing male", `{"voices":[{"voice_id":"eve"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("missing authenticated discovery")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/models":
					_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.6"}]}`))
				case "/v1/tts/voices":
					_, _ = w.Write([]byte(tc.voices))
				default:
					t.Error("unexpected discovery endpoint")
				}
			}))
			defer server.Close()
			models, err := publisher.Discover(context.Background(), server.Client(), publisher.Provider{Slug: "xai", BaseURL: server.URL + "/v1", APIKey: "test-key", Discovery: providerconfig.DiscoveryXAIModels})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"/v1/models", "/v1/tts/voices"}) {
				t.Fatalf("discovery calls = %v", calls)
			}
			speech := false
			chat := false
			for _, m := range models {
				speech = speech || m.UpstreamModelID == "tts"
				chat = chat || m.UpstreamModelID == "grok-4.6"
			}
			if speech != tc.wantSpeech || !chat {
				t.Fatalf("discovered models = %+v", models)
			}
		})
	}
}
