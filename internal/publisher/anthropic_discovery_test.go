package publisher_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

// anthropicOpus55Row is one row of Anthropic's `GET /v1/models` in the
// documented shape (https://platform.claude.com/docs/en/api/models/list),
// capability tree and all, as the account list returns it.
const anthropicOpus55Row = `{
	"type": "model",
	"id": "claude-opus-5-5",
	"display_name": "Claude Opus 5.5",
	"created_at": "2026-09-22T00:00:00Z",
	"deprecated_at": null,
	"retires_at": null,
	"lifecycle": "active",
	"line": "opus",
	"max_input_tokens": 1000000,
	"max_tokens": 128000,
	"capabilities": {
		"batch": {"supported": true},
		"citations": {"supported": true},
		"code_execution": {"supported": true},
		"context_management": {
			"clear_thinking_20251015": {"supported": true},
			"clear_tool_uses_20250919": {"supported": true},
			"compact_20260112": {"supported": true},
			"supported": true
		},
		"effort": {
			"high": {"supported": true},
			"low": {"supported": true},
			"max": {"supported": true},
			"medium": {"supported": true},
			"supported": true,
			"xhigh": {"supported": true}
		},
		"image_input": {"supported": true},
		"pdf_input": {"supported": true},
		"server_tools": {"code_execution": {"supported": true}, "supported": true, "web_search": {"supported": true}},
		"structured_outputs": {"supported": true},
		"thinking": {"supported": true, "types": {"adaptive": {"supported": true}, "disabled": {"supported": false}, "enabled": {"supported": false}}}
	}
}`

// anthropicHaiku45Row takes no effort: Anthropic documents `effort` as an
// error on Claude Haiku 4.5.
const anthropicHaiku45Row = `{
	"type": "model",
	"id": "claude-haiku-4-5-20251001",
	"display_name": "Claude Haiku 4.5",
	"created_at": "2025-10-15T00:00:00Z",
	"deprecated_at": null,
	"retires_at": null,
	"lifecycle": "active",
	"line": "haiku",
	"max_input_tokens": 200000,
	"max_tokens": 64000,
	"capabilities": {
		"effort": {
			"high": {"supported": false},
			"low": {"supported": false},
			"max": {"supported": false},
			"medium": {"supported": false},
			"supported": false,
			"xhigh": null
		},
		"image_input": {"supported": true},
		"thinking": {"supported": true, "types": {"adaptive": {"supported": false}, "disabled": {"supported": true}, "enabled": {"supported": true}}}
	}
}`

// anthropicModelListServer serves the pages keyed by the `after_id` that asks
// for them ("" is the first page), checking that every request authenticates
// exactly as the Messages adapter does.
func anthropicModelListServer(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != providerconfig.AnthropicAPIVersion {
			t.Errorf("x-api-key = %q, anthropic-version = %q", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Errorf("Anthropic was sent an Authorization header: %q", authorization)
		}
		if r.URL.Query().Get("limit") != "1000" {
			t.Errorf("limit = %q, want the documented maximum", r.URL.Query().Get("limit"))
		}
		page, ok := pages[r.URL.Query().Get("after_id")]
		if !ok {
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(server.Close)
	return server
}

func anthropicProvider(base string) publisher.Provider {
	return publisher.Provider{
		Slug: "anthropic", BaseURL: base, APIKey: "test-key",
		Discovery: providerconfig.DiscoveryAnthropicModels, Protocol: providerconfig.ProtocolAnthropicMessages,
	}
}

func TestAnthropicDiscoveryPagesTheAuthenticatedModelsAPI(t *testing.T) {
	server := anthropicModelListServer(t, map[string]string{
		"": `{"data":[` + anthropicOpus55Row + `,
			{"type":"model","id":"claude-sonnet-4-5-20250929","display_name":"Claude Sonnet 4.5","created_at":"2025-09-29T00:00:00Z","deprecated_at":"2026-09-30T00:00:00Z","retires_at":"2026-11-30T00:00:00Z","lifecycle":"deprecated","line":"sonnet","max_input_tokens":200000,"max_tokens":64000,"capabilities":null}
		],"first_id":"claude-opus-5-5","has_more":true,"last_id":"claude-sonnet-4-5-20250929"}`,
		"claude-sonnet-4-5-20250929": `{"data":[` + anthropicHaiku45Row + `,
			{"type":"model","id":"claude-3-haiku-20240307","display_name":"Claude Haiku 3","created_at":"2024-03-07T00:00:00Z","deprecated_at":"2026-02-19T00:00:00Z","retires_at":"2026-04-20T00:00:00Z","lifecycle":"retired","line":"haiku","max_input_tokens":200000,"max_tokens":4096,"capabilities":null}
		],"first_id":"claude-haiku-4-5-20251001","has_more":false,"last_id":"claude-3-haiku-20240307"}`,
	})

	models, err := publisher.Discover(context.Background(), server.Client(), anthropicProvider(server.URL+"/v1"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var ids []string
	byID := map[string]publisher.DiscoveredModel{}
	for _, model := range models {
		ids = append(ids, model.UpstreamModelID)
		byID[model.UpstreamModelID] = model
	}
	// Both pages; the deprecated row kept for attribution to judge; the
	// retired row skipped.
	if want := []string{"claude-haiku-4-5-20251001", "claude-opus-5-5", "claude-sonnet-4-5-20250929"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("models = %v, want %v", ids, want)
	}

	opus := byID["claude-opus-5-5"].Observed
	if opus == nil || opus.DisplayName == nil || *opus.DisplayName != "Claude Opus 5.5" ||
		opus.CreatedAt == nil || opus.ContextTokens == nil || *opus.ContextTokens != 1000000 ||
		opus.MaxOutputTokens == nil || *opus.MaxOutputTokens != 128000 || opus.ReasoningEfforts == nil {
		t.Fatalf("claude-opus-5-5 observed = %+v", opus)
	}
	if got, want := *opus.ReasoningEfforts, contract.ReasoningEfforts(); !reflect.DeepEqual(got, want) {
		t.Errorf("claude-opus-5-5 efforts = %v, want the contract's %v (xhigh and max are not Kaana's to send)", got, want)
	}
	// Anthropic states nothing in the inventory's tool, parameter or modality
	// vocabulary, so nothing is inferred into it.
	if opus.SupportsTools != nil || opus.AcceptedParameters != nil || opus.InputModalities != nil || opus.OutputModalities != nil || opus.ListPrice != nil {
		t.Errorf("claude-opus-5-5 observed carries a field Anthropic did not state: %+v", opus)
	}

	haiku := byID["claude-haiku-4-5-20251001"].Observed
	if haiku == nil || haiku.ReasoningEfforts == nil || len(*haiku.ReasoningEfforts) != 0 {
		t.Errorf("claude-haiku-4-5-20251001 efforts = %+v, want the stated empty set", haiku)
	}

	sonnet := byID["claude-sonnet-4-5-20250929"].Observed
	if sonnet == nil || sonnet.ReasoningEfforts != nil || sonnet.ContextTokens == nil || *sonnet.ContextTokens != 200000 {
		t.Errorf("claude-sonnet-4-5-20250929 observed = %+v, want limits and no effort statement (capabilities null)", sonnet)
	}
}

// TestAnthropicObservationsAreLenient: a metadata field Kaana cannot read
// makes the catalogue say less; it never withdraws the model.
func TestAnthropicObservationsAreLenient(t *testing.T) {
	server := anthropicModelListServer(t, map[string]string{
		"": `{"data":[
			{"type":"model","id":"claude-opus-5","display_name":"","created_at":"1970-01-01T00:00:00Z","max_input_tokens":"1M","max_tokens":0,
			 "capabilities":{"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":null},"high":{"supported":true}}}},
			{"type":"model","id":"claude-sonnet-5","created_at":"not a date","capabilities":{"effort":{"supported":true,"low":{"supported":true},"high":{"supported":true}}}}
		],"has_more":false,"last_id":"claude-sonnet-5"}`,
	})
	models, err := publisher.Discover(context.Background(), server.Client(), anthropicProvider(server.URL+"/v1"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	for _, model := range models {
		if model.Observed != nil {
			t.Errorf("%s observed = %+v, want nothing: an epoch release date, an unreadable limit and an incomplete effort statement are all unknown", model.UpstreamModelID, model.Observed)
		}
	}
}

func TestAnthropicDiscoveryRefusesAMalformedCatalogue(t *testing.T) {
	row := func(id string) string { return `{"type":"model","id":"` + id + `"}` }
	for name, pages := range map[string]map[string]string{
		// Empty pages, so only the cursor itself can stop a loop.
		"repeated cursor": {
			"":  `{"data":[` + row("claude-opus-5-5") + `],"has_more":true,"last_id":"a"}`,
			"a": `{"data":[],"has_more":true,"last_id":"b"}`,
			"b": `{"data":[],"has_more":true,"last_id":"a"}`,
		},
		"more without a cursor": {"": `{"data":[` + row("claude-opus-5-5") + `],"has_more":true,"last_id":null}`},
		"duplicate id": {
			"":                `{"data":[` + row("claude-opus-5-5") + `],"has_more":true,"last_id":"claude-opus-5-5"}`,
			"claude-opus-5-5": `{"data":[` + row("claude-opus-5-5") + `],"has_more":false}`,
		},
		"blank id":         {"": `{"data":[` + row(" claude-opus-5-5") + `],"has_more":false}`},
		"not a model":      {"": `{"data":[{"type":"file","id":"file_011"}],"has_more":false}`},
		"no models":        {"": `{"data":[],"has_more":false}`},
		"only retired":     {"": `{"data":[{"type":"model","id":"claude-3-haiku-20240307","lifecycle":"retired"}],"has_more":false}`},
		"wrong shape":      {"": `{"data":[{"type":"model","id":7}],"has_more":false}`},
		"not the list doc": {"": `{"models":[` + row("claude-opus-5-5") + `]}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := anthropicModelListServer(t, pages)
			if _, err := publisher.Discover(context.Background(), server.Client(), anthropicProvider(server.URL+"/v1")); err == nil {
				t.Fatal("Discover accepted a malformed Anthropic catalogue")
			}
		})
	}
}

// TestAnthropicDiscoveryRedactsItsKeyFromARefusal: Anthropic's error echoes
// nothing today, but an upstream that quoted the header must not put the key
// into a publisher log line.
func TestAnthropicDiscoveryRedactsItsKeyFromARefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: ` + r.Header.Get("x-api-key") + `"},"request_id":"req_011"}`))
	}))
	t.Cleanup(server.Close)

	_, err := publisher.Discover(context.Background(), server.Client(), anthropicProvider(server.URL+"/v1"))
	if err == nil {
		t.Fatal("a 401 was accepted as a model list")
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatalf("the discovery key reached the error: %v", err)
	}
	// The control: the upstream did echo it, and the status survives.
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("the refusal lost its status: %v", err)
	}
}

func TestAnthropicModelListIsReadOnlyForTheAnthropicSlug(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a credential was sent to %s", r.URL)
	}))
	t.Cleanup(server.Close)
	target := anthropicProvider(server.URL + "/v1")
	target.Slug = "bedrock"
	if _, err := publisher.Discover(context.Background(), server.Client(), target); err == nil {
		t.Fatal("Anthropic's model list was read for another slug")
	}
}
