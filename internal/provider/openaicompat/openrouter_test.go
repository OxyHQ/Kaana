package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// TestOpenRouterProviderPolicyOnTheUpstreamWire drives Translate and Stream
// together against a fake that speaks the real Chat Completions wire. Inspecting
// the Call alone would prove serialization but not that these are the bytes the
// HTTP adapter actually sends.
func TestOpenRouterProviderPolicyOnTheUpstreamWire(t *testing.T) {
	type wireCase struct {
		slug       contract.ProviderSlug
		wantPolicy bool
		stream     bool
	}
	testCases := []wireCase{
		{slug: "openrouter", wantPolicy: true, stream: false},
		{slug: "openrouter", wantPolicy: true, stream: true},
		// A caller may configure another valid slug with this protocol. It must
		// not inherit policy by being absent from the built-in table.
		{slug: "custom-compatible", wantPolicy: false, stream: false},
	}
	for slug, endpoint := range providerconfig.Known {
		if endpoint.Protocol == providerconfig.ProtocolOpenAICompatible && slug != "openrouter" {
			testCases = append(testCases, wireCase{slug: slug})
		}
	}

	for _, testCase := range testCases {
		name := string(testCase.slug)
		if testCase.stream {
			name += "/stream"
		}
		t.Run(name, func(t *testing.T) {
			received := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				received <- body
				if testCase.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
						"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
						"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
						"data: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id":"chatcmpl-policy-fake",
					"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":1,"completion_tokens":1}
				}`))
			}))
			t.Cleanup(upstream.Close)

			baseURL := upstream.URL
			var client *http.Client
			if testCase.slug == "openrouter" {
				baseURL = providerconfig.Known["openrouter"].BaseURL
				client = openRouterFakeClient(t, upstream.URL)
			}
			adapter, err := New(Config{
				Provider:     testCase.slug,
				BaseURL:      baseURL,
				Declarations: provider.DeclareKeys([]string{fakeAPIKey}),
				HTTPClient:   client,
			})
			if err != nil {
				t.Fatalf("building the %s adapter: %v", testCase.slug, err)
			}
			request := requestWith([]contract.Message{{
				Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")},
			}})
			request.Stream = testCase.stream
			call, err := adapter.Translate(request, provider.Route{
				DeploymentID:    "dep_policy_test",
				Provider:        testCase.slug,
				ModelReference:  "openai/gpt-5@2026-05-01",
				UpstreamModelID: "gpt-5-2026-05-01",
			})
			if err != nil {
				t.Fatalf("translating for %s: %v", testCase.slug, err)
			}
			if _, err := adapter.Stream(context.Background(), call, silentEmitter{}); err != nil {
				t.Fatalf("sending through %s: %v", testCase.slug, err)
			}

			var wire map[string]json.RawMessage
			if err := json.Unmarshal(<-received, &wire); err != nil {
				t.Fatalf("the upstream wire body is not JSON: %v", err)
			}
			policy, present := wire["provider"]
			if present != testCase.wantPolicy {
				t.Fatalf("provider field present = %v, expected %v: %s", present, testCase.wantPolicy, policy)
			}
			if testCase.wantPolicy {
				assertExactOpenRouterProviderPolicy(t, policy)
			}
		})
	}
}

// TestAnUnknownCallerProviderObjectCannotDowngradeOpenRouterPolicy pins the
// compatibility tradeoff at the Oxy boundary. Inbound envelopes deliberately
// accept unknown additive fields, but they are not a raw-body passthrough. A
// caller-supplied provider object is discarded by contract decoding and Kaana
// constructs its own stronger object during translation.
func TestAnUnknownCallerProviderObjectCannotDowngradeOpenRouterPolicy(t *testing.T) {
	original, err := json.Marshal(requestWith([]contract.Message{{
		Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("hi")},
	}}))
	if err != nil {
		t.Fatalf("encoding the caller request: %v", err)
	}
	var caller map[string]any
	if err := json.Unmarshal(original, &caller); err != nil {
		t.Fatalf("decoding the caller request: %v", err)
	}
	caller["provider"] = map[string]any{
		"zdr":                false,
		"data_collection":    "allow",
		"require_parameters": false,
		"order":              []string{"unreviewed-upstream"},
	}
	downgraded, err := json.Marshal(caller)
	if err != nil {
		t.Fatalf("encoding the downgrade attempt: %v", err)
	}
	var decoded contract.Request
	if err := json.Unmarshal(downgraded, &decoded); err != nil {
		t.Fatalf("the additive unknown field unexpectedly broke contract decoding: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("the valid request was changed by its ignored unknown field: %v", err)
	}

	adapter, err := New(Config{Provider: "openrouter", BaseURL: providerconfig.Known["openrouter"].BaseURL})
	if err != nil {
		t.Fatalf("building the OpenRouter adapter: %v", err)
	}
	call, err := adapter.Translate(&decoded, provider.Route{
		DeploymentID:    "dep_policy_test",
		Provider:        "openrouter",
		ModelReference:  "openai/gpt-5@2026-05-01",
		UpstreamModelID: "gpt-5-2026-05-01",
	})
	if err != nil {
		t.Fatalf("translating the caller request: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &wire); err != nil {
		t.Fatalf("the translated body is not JSON: %v", err)
	}
	assertExactOpenRouterProviderPolicy(t, wire["provider"])
}

func TestAdapterRejectsAnOpenRouterEndpointIdentityMismatch(t *testing.T) {
	for _, config := range []Config{
		{Provider: "custom-compatible", BaseURL: "https://openrouter.ai/api/v1"},
		{Provider: "custom-compatible", BaseURL: "https://user@OPENROUTER.AI.:443/api/./v1/"},
		{Provider: "openrouter", BaseURL: "https://api.groq.com/openai/v1"},
		{Provider: "openrouter", BaseURL: "https://www.openrouter.ai/api/v1"},
		{Provider: "openrouter", BaseURL: "https://api.openrouter.ai/api/v1"},
		{Provider: "openrouter", BaseURL: "https://openrouter.ai/api//v1"},
	} {
		if _, err := New(config); err == nil {
			t.Errorf("adapter accepted provider %q on endpoint %q", config.Provider, config.BaseURL)
		}
	}

	if _, err := New(Config{Provider: "custom-compatible", BaseURL: "https://openrouter.ai.example.com/api/v1"}); err != nil {
		t.Errorf("a similar but unrelated endpoint was reserved: %v", err)
	}
}

func assertExactOpenRouterProviderPolicy(t *testing.T, encoded json.RawMessage) {
	t.Helper()
	var policy map[string]any
	if err := json.Unmarshal(encoded, &policy); err != nil {
		t.Fatalf("the OpenRouter provider policy is not an object: %v", err)
	}
	if len(policy) != 3 {
		t.Fatalf("the OpenRouter provider policy has %d fields, expected exactly 3: %v", len(policy), policy)
	}
	if policy["zdr"] != true || policy["data_collection"] != "deny" || policy["require_parameters"] != true {
		t.Fatalf("the OpenRouter provider policy is weaker than required: %v", policy)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// openRouterFakeClient keeps the configured identity canonical while sending
// the request to the in-memory real-wire fake. Changing BaseURL in a test would
// disable the same binding the test is meant to exercise.
func openRouterFakeClient(t *testing.T, upstream string) *http.Client {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parsing the fake upstream URL: %v", err)
	}
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		redirected := request.Clone(request.Context())
		redirected.URL.Scheme = target.Scheme
		redirected.URL.Host = target.Host
		redirected.Host = ""
		return http.DefaultTransport.RoundTrip(redirected)
	})}
}
