package publisher

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// openRouterEmbeddingModels are OpenRouter's own `/models` rows for two
// embedding models, read 2026-10-15 from
// GET https://openrouter.ai/api/v1/models?output_modalities=text,decisions,embeddings
// and trimmed to the fields the publisher reads.
const openRouterEmbeddingModels = `{"data":[
 {"id":"qwen/qwen3-embedding-8b","canonical_slug":"qwen/qwen3-embedding-8b","name":"Qwen: Qwen3 Embedding 8B","created":1761680622,"context_length":32768,
  "architecture":{"modality":"text->embeddings","input_modalities":["text"],"output_modalities":["embeddings"]},
  "pricing":{"prompt":"0.00000001","completion":"0"},"top_provider":{"context_length":32000,"max_completion_tokens":28800},"supported_parameters":[]},
 {"id":"thenlper/gte-large","canonical_slug":"thenlper/gte-large","name":"Thenlper: GTE-Large","created":1763050000,"context_length":512,
  "architecture":{"modality":"text->embeddings","input_modalities":["text"],"output_modalities":["embeddings"]},
  "pricing":{"prompt":"0.00000001","completion":"0"},"top_provider":{"context_length":512},"supported_parameters":[]}
]}`

// openRouterEmbeddingZDR is the zero-retention list's rows for the same model,
// GET https://openrouter.ai/api/v1/endpoints/zdr read 2026-10-15.
const openRouterEmbeddingZDR = `{"data":[
 {"name":"Nebius | qwen/qwen3-embedding-8b","model_id":"qwen/qwen3-embedding-8b","provider_name":"Nebius","tag":"nebius","context_length":32000,"supported_parameters":[],"status":0},
 {"name":"DeepInfra | qwen/qwen3-embedding-8b","model_id":"qwen/qwen3-embedding-8b","provider_name":"DeepInfra","tag":"deepinfra/us","context_length":32768,"supported_parameters":[],"status":0},
 {"name":"DeepInfra | thenlper/gte-large","model_id":"thenlper/gte-large","provider_name":"DeepInfra","tag":"deepinfra","context_length":512,"supported_parameters":[],"status":0}
]}`

// TestAnAttributedOpenRouterEmbeddingModelIsPublished: OpenRouter lists
// embedding models only when `output_modalities` asks for them, so the
// discovery request must; once listed, an attributed embedding model with a
// zero-retention endpoint becomes a deployment (the OpenAI-compatible adapter
// executes embeddings under `openrouter`), and an unattributed one is dropped
// exactly as an unattributed chat model is.
func TestAnAttributedOpenRouterEmbeddingModelIsPublished(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			if !slices.Contains(strings.Split(r.URL.Query().Get("output_modalities"), ","), "embeddings") {
				// OpenRouter's documented default omits embedding models.
				_, _ = w.Write([]byte(`{"data":[{"id":"google/gemini-3.7-flash","canonical_slug":"google/gemini-3.7-flash-20260813"}]}`))
				return
			}
			_, _ = w.Write([]byte(openRouterEmbeddingModels))
		case "/v1/endpoints/zdr":
			_, _ = w.Write([]byte(openRouterEmbeddingZDR))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	attribution, err := ParseAttribution([]byte(`{"attribution":{"openrouter":{"qwen/qwen3-embedding-8b":"qwen/qwen3-embedding-8b"}}}`))
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	var logs bytes.Buffer
	store := &fakeStore{}
	publisher, err := New(Config{
		Providers:   []Provider{{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"}},
		Attribution: attribution,
		Store:       store,
		Client:      server.Client(),
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("wiring the publisher: %v", err)
	}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	published := parseSnapshot(t, store.written()[0])
	if len(published.Deployments) != 1 {
		t.Fatalf("published %d deployments, want the one attributed embedding model: %+v", len(published.Deployments), published.Deployments)
	}
	deployment := published.Deployments[0]
	if deployment.Provider != "openrouter" || deployment.UpstreamModelID != "qwen/qwen3-embedding-8b" || !strings.HasPrefix(string(deployment.ModelReference), "qwen/qwen3-embedding-8b@") {
		t.Fatalf("deployment = %+v", deployment)
	}
	if deployment.Observed == nil || !slices.Equal(deployment.Observed.OutputModalities, []string{"embeddings"}) {
		t.Fatalf("the deployment lost what OpenRouter said it produces: %+v", deployment.Observed)
	}
	if !strings.Contains(logs.String(), "thenlper/gte-large") {
		t.Errorf("the unattributed embedding model was not named when dropped:\n%s", logs.String())
	}
}
