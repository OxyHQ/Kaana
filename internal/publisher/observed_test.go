package publisher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// openRouterModelsFixture is the shape of OpenRouter's documented
// `GET /api/v1/models` entry, trimmed to one model, with the fields Kaana does
// not read left in so the decoder is exercised against the real envelope.
const openRouterModelsFixture = `{"data":[{
  "id":"openai/gpt-oss-120b",
  "canonical_slug":"openai/gpt-oss-120b",
  "name":"OpenAI: gpt-oss-120b",
  "created":1754414231,
  "description":"An open-weight model.",
  "context_length":131072,
  "architecture":{"modality":"text->text","input_modalities":["text"],"output_modalities":["text"],"tokenizer":"GPT"},
  "pricing":{"prompt":"0.000000072","completion":"0.00000028","request":"0","image":"0"},
  "top_provider":{"context_length":131072,"max_completion_tokens":32768,"is_moderated":false},
  "per_request_limits":null,
  "supported_parameters":["frequency_penalty","include_reasoning","max_tokens","reasoning","response_format","seed","stop","temperature","tool_choice","tools","top_p"]
},{
  "id":"meta-llama/llama-3.1-8b-instruct",
  "name":"Meta: Llama 3.1 8B Instruct",
  "created":1721692800,
  "context_length":16384,
  "architecture":{"input_modalities":["text"],"output_modalities":["text"]},
  "pricing":{"prompt":"0.000000015","completion":"0.00000002"},
  "top_provider":{"context_length":16384,"max_completion_tokens":null},
  "supported_parameters":["max_tokens","temperature","top_p","stop"]
}]}`

// groqModelsFixture is Groq's documented `GET /openai/v1/models` entry: no
// name, no parameters, no price — only what Groq publishes.
const groqModelsFixture = `{"object":"list","data":[{
  "id":"openai/gpt-oss-120b","object":"model","created":1754408224,"owned_by":"OpenAI",
  "active":true,"context_window":131072,"public_apps":null,"max_completion_tokens":65536
}]}`

func serveModelList(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func intPointer(value int) *int { return &value }

func TestDiscoveryKeepsWhatOpenRouterPublishesAboutEachModel(t *testing.T) {
	server := serveModelList(t, openRouterModelsFixture)
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("discovered %d models", len(models))
	}
	byID := map[string]*inventory.Observed{}
	for _, model := range models {
		byID[model.UpstreamModelID] = model.Observed
	}

	oss := byID["openai/gpt-oss-120b"]
	if oss == nil {
		t.Fatal("the richly described model kept no metadata")
	}
	created := contract.NewTimestamp(time.Unix(1754414231, 0))
	allEfforts := contract.ReasoningEfforts()
	tools := true
	name := "OpenAI: gpt-oss-120b"
	want := inventory.Observed{
		DisplayName:      &name,
		CreatedAt:        &created,
		ContextTokens:    intPointer(131072),
		MaxOutputTokens:  intPointer(32768),
		InputModalities:  []string{"text"},
		OutputModalities: []string{"text"},
		SupportsTools:    &tools,
		ReasoningEfforts: &allEfforts,
	}
	if oss.ListPrice == nil || oss.ListPrice.Currency != "USD" || oss.ListPrice.Input != "0.072" || oss.ListPrice.Output != "0.28" {
		t.Errorf("list price = %+v, want USD 0.072 / 0.28 per million", oss.ListPrice)
	}
	got := *oss
	got.ListPrice = nil
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Errorf("observed\n got %s\nwant %s", gotJSON, wantJSON)
	}

	llama := byID["meta-llama/llama-3.1-8b-instruct"]
	if llama == nil || llama.SupportsTools == nil || *llama.SupportsTools {
		t.Errorf("a present parameter list without tools must report tools unsupported: %+v", llama)
	}
	if llama.ReasoningEfforts == nil || len(*llama.ReasoningEfforts) != 0 {
		t.Errorf("a present parameter list without reasoning must report [] efforts: %+v", llama.ReasoningEfforts)
	}
	if llama.MaxOutputTokens != nil {
		t.Errorf("a null max_completion_tokens became %d instead of staying absent", *llama.MaxOutputTokens)
	}
}

func TestDiscoveryKeepsOnlyWhatGroqPublishes(t *testing.T) {
	server := serveModelList(t, groqModelsFixture)
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "groq", BaseURL: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	observed := models[0].Observed
	if observed == nil || observed.ContextTokens == nil || *observed.ContextTokens != 131072 ||
		observed.MaxOutputTokens == nil || *observed.MaxOutputTokens != 65536 || observed.CreatedAt == nil {
		t.Fatalf("observed = %+v", observed)
	}
	// Unknown stays unknown: Groq publishes no parameter list, so neither tool
	// support nor reasoning control may be inferred for it.
	if observed.DisplayName != nil || observed.SupportsTools != nil || observed.ReasoningEfforts != nil ||
		observed.InputModalities != nil || observed.ListPrice != nil {
		t.Errorf("Groq's list said nothing about these, yet they were filled: %+v", observed)
	}
}

func TestMalformedMetadataNeverWithdrawsAModel(t *testing.T) {
	server := serveModelList(t, `{"data":[
	  {"id":"a","name":"  padded  ","created":"yesterday","context_length":-1,
	   "architecture":{"input_modalities":["text","Image!"]},
	   "pricing":{"prompt":"-1","completion":"-1"},"supported_parameters":"tools"},
	  {"id":"b","created":0,"owned_by":"x"}
	]}`)
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("a metadata change failed discovery: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("discovered %d models, want both", len(models))
	}
	for _, model := range models {
		if model.Observed != nil {
			t.Errorf("%s kept unreadable metadata: %+v", model.UpstreamModelID, model.Observed)
		}
	}
}

func TestAPriceShapedFieldIsReadOnlyWhereTheCatalogueDocumentsIt(t *testing.T) {
	server := serveModelList(t, openRouterModelsFixture)
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "custom-compatible", BaseURL: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, model := range models {
		if model.Observed == nil {
			t.Fatalf("control: %s kept no metadata at all", model.UpstreamModelID)
		}
		if model.Observed.ListPrice != nil {
			t.Errorf("%s: a `pricing` field on an endpoint that does not document its unit became a list price", model.UpstreamModelID)
		}
	}
}

func TestObservedMetadataReachesTheReaderWithoutMovingTheSnapshotID(t *testing.T) {
	attribution, err := ParseAttribution([]byte(`{"attribution":{
	  "openrouter":{"openai/gpt-oss-120b":"openai/gpt-oss-120b"},
	  "groq":{"openai/gpt-oss-120b":"openai/gpt-oss-120b"}}}`))
	if err != nil {
		t.Fatalf("attribution: %v", err)
	}
	discover := func(slug contract.ProviderSlug, fixture string) Discovery {
		server := serveModelList(t, fixture)
		models, err := Discover(context.Background(), server.Client(), Provider{Slug: slug, BaseURL: server.URL + "/v1", APIKey: "k"})
		if err != nil {
			t.Fatalf("Discover %s: %v", slug, err)
		}
		return Discovery{Provider: Provider{Slug: slug}, Models: models}
	}
	at := time.Now()
	withMetadata, err := BuildSnapshot([]Discovery{discover("openrouter", openRouterModelsFixture), discover("groq", groqModelsFixture)}, attribution, nil, at)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	bare := []Discovery{
		{Provider: Provider{Slug: "openrouter"}, Models: []DiscoveredModel{{UpstreamModelID: "openai/gpt-oss-120b"}}},
		{Provider: Provider{Slug: "groq"}, Models: []DiscoveredModel{{UpstreamModelID: "openai/gpt-oss-120b"}}},
	}
	withoutMetadata, err := BuildSnapshot(bare, attribution, nil, at)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if withMetadata.SnapshotID != withoutMetadata.SnapshotID {
		t.Errorf("catalogue metadata moved the routing id: %s vs %s", withMetadata.SnapshotID, withoutMetadata.SnapshotID)
	}

	loaded, err := inventory.Parse(withMetadata.Body, inventory.DefaultMaxSnapshotAge)
	if err != nil {
		t.Fatalf("the reader refused the published snapshot: %v", err)
	}
	catalogue := loaded.Catalogue()
	if len(catalogue) != 1 {
		t.Fatalf("catalogue = %+v", catalogue)
	}
	entry := catalogue[0]
	// Groq reports 65536 output tokens, OpenRouter 32768: the limit both honour.
	if entry.MaxOutputTokens == nil || *entry.MaxOutputTokens != 32768 || entry.ContextTokens == nil || *entry.ContextTokens != 131072 {
		t.Errorf("limits = %v / %v", entry.ContextTokens, entry.MaxOutputTokens)
	}
	// Groq is silent on tools and reasoning, so it abstains; OpenRouter decides.
	if entry.SupportsTools == nil || !*entry.SupportsTools || entry.ReasoningEfforts == nil || len(*entry.ReasoningEfforts) != 3 {
		t.Errorf("capabilities = tools %v efforts %v", entry.SupportsTools, entry.ReasoningEfforts)
	}
	if len(entry.ListPrices) != 1 || entry.ListPrices[0].Provider != "openrouter" || entry.ListPrices[0].Input != "0.072" {
		t.Errorf("list prices = %+v", entry.ListPrices)
	}
	// Groq's creation instant is earlier than OpenRouter's listing.
	if entry.CreatedAt == nil || *entry.CreatedAt != contract.NewTimestamp(time.Unix(1754408224, 0)) {
		t.Errorf("createdAt = %v", entry.CreatedAt)
	}
}
