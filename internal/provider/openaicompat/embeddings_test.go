package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// embeddingSlugs splits the built-in OpenAI-compatible slugs by the one fact
// that decides whether this adapter builds an embeddings call: the capability
// table. Deriving both halves from it means a slug added to the table is
// exercised on the wire the same day, and a slug left out is proven refused.
func embeddingSlugs() (served, refused []contract.ProviderSlug) {
	for _, slug := range compatibleBuiltInSlugs() {
		if slices.Contains(providerconfig.ExecutableAPIFormats(slug, providerconfig.ProtocolOpenAICompatible), contract.APIFormatEmbeddings) {
			served = append(served, slug)
		} else {
			refused = append(refused, slug)
		}
	}
	return served, refused
}

func embeddingEnvelope(text string) *contract.Request {
	request := requestWith(nil)
	request.Modality = contract.ModalityEmbedding
	request.Input = contract.Input{Format: contract.InputText, Text: &text}
	request.Stream = false
	request.Client.APIFormat = contract.APIFormatEmbeddings
	request.Client.Endpoint = "/v1/embeddings"
	return request
}

func embeddingAdapter(t *testing.T, slug contract.ProviderSlug, upstreamURL string) *Adapter {
	t.Helper()
	baseURL := upstreamURL
	var client *http.Client
	if reviewedBaseURL, identityBound := identityBoundTestBaseURL(slug); identityBound {
		baseURL = reviewedBaseURL
		client = identityBoundFakeClient(t, upstreamURL)
	}
	adapter, err := New(Config{
		Provider:     slug,
		BaseURL:      baseURL,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey}),
		HTTPClient:   client,
	})
	if err != nil {
		t.Fatalf("building the %s adapter: %v", slug, err)
	}
	return adapter
}

// TestTheCapabilityTableListsEmbeddingsForSiliconFlowAndOpenRouter pins the
// table's current answer so the derived tests below cannot pass vacuously.
func TestTheCapabilityTableListsEmbeddingsForSiliconFlowAndOpenRouter(t *testing.T) {
	served, _ := embeddingSlugs()
	if fmt.Sprint(served) != "[openrouter siliconflow]" {
		t.Fatalf("embeddings are served for %v, expected [openrouter siliconflow]", served)
	}
}

// TestEmbeddingsOnTheUpstreamWire drives Translate and Stream together against
// a fake speaking the real OpenAI embeddings wire, for every slug the table
// lists: the request reaches `{base}/embeddings` with the upstream model id,
// the input and the fixed dimension, OpenRouter's body carries exactly its
// non-negotiable routing policy and no other slug's carries one, and the
// answer comes back as a 1024-dimension embedding with provider-reported units.
func TestEmbeddingsOnTheUpstreamWire(t *testing.T) {
	served, _ := embeddingSlugs()
	for _, slug := range served {
		t.Run(string(slug), func(t *testing.T) {
			type exchange struct {
				path string
				body []byte
			}
			received := make(chan exchange, 1)
			vector := make([]float64, provider.EmbeddingDimension)
			vector[0] = 0.5
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				received <- exchange{path: request.URL.Path, body: body}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"object":"list","model":"upstream-echo","data":[{"object":"embedding","index":0,"embedding":%s}],"usage":{"prompt_tokens":4,"total_tokens":4}}`, mustJSON(t, vector))
			}))
			t.Cleanup(upstream.Close)

			adapter := embeddingAdapter(t, slug, upstream.URL)
			call, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{
				DeploymentID:    "dep_embedding_wire",
				Provider:        slug,
				ModelReference:  "qwen/qwen3-embedding-8b@2026-10-15",
				UpstreamModelID: "qwen/qwen3-embedding-8b",
			})
			if err != nil {
				t.Fatalf("translating for %s: %v", slug, err)
			}
			outcome, err := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
			if err != nil {
				t.Fatalf("sending through %s: %v", slug, err)
			}

			sent := <-received
			if want := "/embeddings"; len(sent.path) < len(want) || sent.path[len(sent.path)-len(want):] != want {
				t.Fatalf("the request went to %q, not the embeddings endpoint", sent.path)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(sent.body, &wire); err != nil {
				t.Fatalf("the upstream wire body is not JSON: %v", err)
			}
			if string(wire["model"]) != `"qwen/qwen3-embedding-8b"` || string(wire["input"]) != `"embed me"` || string(wire["dimensions"]) != "1024" {
				t.Fatalf("the embeddings body is not the reviewed one: %s", sent.body)
			}
			policy, present := wire["provider"]
			if present != (slug == "openrouter") {
				t.Fatalf("provider field present = %v on %s: %s", present, slug, policy)
			}
			if present {
				assertPinnedOpenRouterProviderPolicy(t, policy, []string{"deepinfra", "nebius"})
			}

			if outcome.Embedding == nil || outcome.Embedding.Dimension != provider.EmbeddingDimension || len(outcome.Embedding.Data) != 1 || outcome.Embedding.Data[0].Embedding[0] != 0.5 {
				t.Fatalf("the embedding did not come back intact: %+v", outcome.Embedding)
			}
			if outcome.UsageSource != contract.UsageProviderReported {
				t.Fatalf("usage source = %q", outcome.UsageSource)
			}
			want := []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 4}, {Unit: contract.UnitEmbeddings, Quantity: 1}}
			if fmt.Sprint(outcome.Units) != fmt.Sprint(want) {
				t.Fatalf("units = %v, want %v", outcome.Units, want)
			}
		})
	}
}

// TestAnEmbeddingOfAnotherDimensionIsAProviderError: the dimension is the one
// thing a caller's vector index depends on, so an upstream that ignored
// `dimensions` is refused rather than relayed.
func TestAnEmbeddingOfAnotherDimensionIsAProviderError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":[{"index":0,"embedding":%s}],"usage":{"prompt_tokens":4,"total_tokens":4}}`, mustJSON(t, make([]float64, 4096)))
	}))
	t.Cleanup(upstream.Close)
	adapter := embeddingAdapter(t, "openrouter", upstream.URL)
	call, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{DeploymentID: "dep_embedding_wire", Provider: "openrouter", ModelReference: "qwen/qwen3-embedding-8b@2026-10-15", UpstreamModelID: "qwen/qwen3-embedding-8b"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
	var upstreamErr provider.ErrUpstream
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != contract.CodeProviderError {
		t.Fatalf("a 4096-dimension answer was not a provider error: %v", err)
	}
	if outcome.Embedding != nil {
		t.Fatal("a wrong-dimension embedding was relayed")
	}
}

// TestEmbeddingsAreRefusedWhereTheTableListsNone: every other built-in slug
// refuses an embedding request in Translate, before anything is sent.
func TestEmbeddingsAreRefusedWhereTheTableListsNone(t *testing.T) {
	_, refused := embeddingSlugs()
	if len(refused) == 0 {
		t.Fatal("no slug without embeddings: the refusal below would prove nothing")
	}
	for _, slug := range refused {
		t.Run(string(slug), func(t *testing.T) {
			adapter := embeddingAdapter(t, slug, "https://upstream.invalid")
			call, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{DeploymentID: "dep_embedding", Provider: slug, ModelReference: "qwen/qwen3-embedding-8b@2026-10-15", UpstreamModelID: "qwen3-embedding-8b"})
			var unsupported provider.ErrUnsupported
			if call != nil || !errors.As(err, &unsupported) || unsupported.Code != contract.CodeUnsupportedModality {
				t.Fatalf("%s built an embeddings call: call=%v err=%v", slug, call, err)
			}
		})
	}
}

// TestAnEmbeddingRequestToAModelOfAnotherFamilyIsRefused: where the id names
// the model's family (OpenRouter's `openai/` rows), an embedding request to a
// chat model never reaches the network, and an OpenAI embedding model is
// still served.
func TestAnEmbeddingRequestToAModelOfAnotherFamilyIsRefused(t *testing.T) {
	adapter := embeddingAdapter(t, "openrouter", "https://upstream.invalid")
	_, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{DeploymentID: "dep_chat", Provider: "openrouter", ModelReference: "openai/gpt-5@2026-05-01", UpstreamModelID: "openai/gpt-5"})
	var unsupported provider.ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Code != contract.CodeUnsupportedModality {
		t.Fatalf("an embedding request to a chat model was not refused: %v", err)
	}
	if _, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{DeploymentID: "dep_embedding", Provider: "openrouter", ModelReference: "openai/text-embedding-3-small@2026-10-15", UpstreamModelID: "openai/text-embedding-3-small"}); err != nil {
		t.Fatalf("an OpenAI embedding model was refused: %v", err)
	}
}

// assertPinnedOpenRouterProviderPolicy is the fixed zero-retention policy plus
// a pinned upstream order with no fallback, and nothing else.
func assertPinnedOpenRouterProviderPolicy(t *testing.T, encoded json.RawMessage, order []string) {
	t.Helper()
	var policy struct {
		ZDR               *bool    `json:"zdr"`
		DataCollection    string   `json:"data_collection"`
		RequireParameters *bool    `json:"require_parameters"`
		Only              []string `json:"only"`
		Order             []string `json:"order"`
		AllowFallbacks    *bool    `json:"allow_fallbacks"`
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil || json.Unmarshal(encoded, &policy) != nil {
		t.Fatalf("the OpenRouter provider policy is not an object: %s", encoded)
	}
	if len(fields) != 6 || policy.ZDR == nil || !*policy.ZDR || policy.DataCollection != "deny" ||
		policy.RequireParameters == nil || !*policy.RequireParameters ||
		fmt.Sprint(policy.Only) != fmt.Sprint(order) || fmt.Sprint(policy.Order) != fmt.Sprint(order) ||
		policy.AllowFallbacks == nil || *policy.AllowFallbacks {
		t.Fatalf("the pinned OpenRouter provider policy = %s, want zdr, deny, require_parameters, only=order=%v, no fallbacks", encoded, order)
	}
}

// The control for the pin: an embedding model with no reviewed order keeps
// exactly the fixed policy, so OpenRouter routes it as before.
func TestAnUnpinnedOpenRouterEmbeddingKeepsTheFixedPolicy(t *testing.T) {
	adapter := embeddingAdapter(t, "openrouter", "https://openrouter.ai/api/v1")
	call, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{
		DeploymentID: "dep_embedding_unpinned", Provider: "openrouter",
		ModelReference: "baai/bge-m3@2026-10-15", UpstreamModelID: "baai/bge-m3",
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &wire); err != nil {
		t.Fatal(err)
	}
	assertExactOpenRouterProviderPolicy(t, wire["provider"])
}
