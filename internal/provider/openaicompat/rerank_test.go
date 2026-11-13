package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func rerankEnvelope(query string, parameters *contract.RerankParameters) *contract.Request {
	request := requestWith(nil)
	request.Modality = contract.ModalityText
	request.Input = contract.Input{Format: contract.InputText, Text: &query}
	request.Stream = false
	request.Client.APIFormat = contract.APIFormatRerank
	request.Client.Endpoint = "/v1/rerank"
	request.Rerank = parameters
	return request
}

func rerankRoute() provider.Route {
	return provider.Route{DeploymentID: "dep_rerank_wire", Provider: "openrouter", ModelReference: "cohere/rerank-v3.5@2026-01-01", UpstreamModelID: rerankUpstreamModel}
}

// runRerank drives Translate and Stream against a fake upstream answering with
// the given JSON and returns the body the upstream received.
func runRerank(t *testing.T, answer string, parameters *contract.RerankParameters) (provider.Outcome, []byte, error) {
	t.Helper()
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(upstream.Close)
	adapter := embeddingAdapter(t, "openrouter", upstream.URL)
	call, err := adapter.Translate(rerankEnvelope("q", parameters), rerankRoute())
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	outcome, err := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
	return outcome, received, err
}

func documents(count int) []string {
	out := make([]string, count)
	for index := range out {
		out[index] = "d"
	}
	return out
}

// The table lists rerank for OpenRouter and for no other built-in slug: a slug
// is added by a reviewed line, not by looking like OpenRouter.
func TestOnlyOpenRouterIsListedForRerank(t *testing.T) {
	var served []contract.ProviderSlug
	for _, slug := range compatibleBuiltInSlugs() {
		adapter := embeddingAdapter(t, slug, "http://127.0.0.1:1")
		if provider.Executes(adapter, contract.APIFormatRerank) {
			served = append(served, slug)
			continue
		}
		_, err := adapter.Translate(rerankEnvelope("q", &contract.RerankParameters{Documents: documents(1)}), provider.Route{Provider: slug, UpstreamModelID: "m", ModelReference: "a/b@1"})
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Code != contract.CodeUnsupportedModality {
			t.Errorf("%s: err = %v; a slug the table does not list refuses rerank", slug, err)
		}
	}
	if len(served) != 1 || served[0] != "openrouter" {
		t.Fatalf("rerank is served for %v, expected [openrouter]", served)
	}
}

func TestTheRerankRequestIsTheDocumentedBody(t *testing.T) {
	topN := 2
	_, received, err := runRerank(t, `{"results":[{"index":0,"relevance_score":0.5}]}`, &contract.RerankParameters{Documents: []string{"alpha", "beta"}, TopN: &topN, ReturnDocuments: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(received, &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"model", "query", "documents", "top_n", "provider"} {
		if _, found := body[field]; !found {
			t.Errorf("body %s lacks %q", received, field)
		}
	}
	if _, sent := body["return_documents"]; sent {
		t.Errorf("body %s sends return_documents; the caller's own text is echoed locally", received)
	}
	policy, _ := body["provider"].(map[string]any)
	if policy["zdr"] != true || policy["data_collection"] != "deny" {
		t.Errorf("provider policy = %v; OpenRouter requests are zero-retention", policy)
	}
}

func TestRerankWithoutTopNSendsNone(t *testing.T) {
	_, received, err := runRerank(t, `{"results":[{"index":0,"relevance_score":0.5}]}`, &contract.RerankParameters{Documents: documents(2)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(received), "top_n") {
		t.Errorf("body %s defaults top_n", received)
	}
}

func TestRerankTokenReportingVariants(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		tokens int
	}{
		"usage prompt_tokens":    {`{"results":[{"index":0,"relevance_score":1}],"usage":{"prompt_tokens":21,"total_tokens":21}}`, 21},
		"usage input_tokens":     {`{"results":[{"index":0,"relevance_score":1}],"usage":{"input_tokens":22}}`, 22},
		"meta billed_units":      {`{"results":[{"index":0,"relevance_score":1}],"meta":{"billed_units":{"input_tokens":23,"search_units":1}}}`, 23},
		"meta tokens":            {`{"results":[{"index":0,"relevance_score":1}],"meta":{"tokens":{"input_tokens":24}}}`, 24},
		"a bare total":           {`{"results":[{"index":0,"relevance_score":1}],"usage":{"total_tokens":25}}`, 25},
		"search units only":      {`{"results":[{"index":0,"relevance_score":1}],"usage":{"search_units":1}}`, 0},
		"nothing":                {`{"results":[{"index":0,"relevance_score":1}]}`, 0},
		"documents echoed as {}": {`{"results":[{"index":0,"relevance_score":1,"document":{"text":"d"}}],"usage":{"prompt_tokens":26}}`, 26},
	} {
		t.Run(name, func(t *testing.T) {
			outcome, _, err := runRerank(t, tc.answer, &contract.RerankParameters{Documents: documents(2)})
			if err != nil || outcome.Rerank == nil {
				t.Fatalf("outcome = %+v, err = %v", outcome, err)
			}
			units := map[contract.UsageUnit]int{}
			for _, unit := range outcome.Units {
				units[unit.Unit] = unit.Quantity
			}
			if units[contract.UnitRequests] != 1 || units[contract.UnitInputTokens] != tc.tokens || outcome.Rerank.InputTokens != tc.tokens {
				t.Errorf("units = %v, result tokens = %d; want one request and %d tokens", units, outcome.Rerank.InputTokens, tc.tokens)
			}
			if _, has := units[contract.UnitInputTokens]; has == (tc.tokens == 0) {
				t.Errorf("units = %v; input tokens appear exactly when the provider reported them", units)
			}
			wantSource := contract.UsageProviderReported
			if tc.tokens == 0 {
				wantSource = contract.UsageEstimated
			}
			if outcome.UsageSource != wantSource {
				t.Errorf("source = %s, want %s", outcome.UsageSource, wantSource)
			}
		})
	}
}

func TestRerankOrdersAndCapsTheProvidersAnswer(t *testing.T) {
	topN := 2
	outcome, _, err := runRerank(t, `{"results":[{"index":2,"relevance_score":0.1},{"index":0,"relevance_score":0.9},{"index":1,"relevance_score":0.5}]}`, &contract.RerankParameters{Documents: documents(3), TopN: &topN})
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, ranked := range outcome.Rerank.Results {
		got = append(got, ranked.Index)
	}
	if !slices.Equal(got, []int{0, 1}) {
		t.Errorf("ranking = %v; want the top two, most relevant first", got)
	}
}

func TestARerankAnswerOutsideTheRequestIsAProviderError(t *testing.T) {
	for name, answer := range map[string]string{
		"index past the documents": `{"results":[{"index":2,"relevance_score":1}]}`,
		"negative index":           `{"results":[{"index":-1,"relevance_score":1}]}`,
		"repeated index":           `{"results":[{"index":0,"relevance_score":1},{"index":0,"relevance_score":0.5}]}`,
		"no score":                 `{"results":[{"index":0}]}`,
		"no index":                 `{"results":[{"relevance_score":1}]}`,
		"unreadable":               `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			outcome, _, err := runRerank(t, answer, &contract.RerankParameters{Documents: documents(2)})
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderError || outcome.Rerank != nil {
				t.Fatalf("outcome = %+v, err = %v", outcome, err)
			}
		})
	}
}

func TestAnEmptyRerankAnswerIsAServerError(t *testing.T) {
	outcome, _, err := runRerank(t, `{"results":[],"usage":{"prompt_tokens":5}}`, &contract.RerankParameters{Documents: documents(2)})
	var upstream provider.ErrUpstream
	if !errors.As(err, &upstream) || upstream.Category != contract.UpstreamServerError || outcome.Rerank != nil {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
}

func TestRerankRefusesWhatItCannotSend(t *testing.T) {
	adapter := embeddingAdapter(t, "openrouter", "http://127.0.0.1:1")
	streaming := rerankEnvelope("q", &contract.RerankParameters{Documents: documents(1)})
	streaming.Stream = true
	noParameters := rerankEnvelope("q", nil)
	for name, request := range map[string]*contract.Request{"streaming": streaming, "no rerank member": noParameters} {
		if _, err := adapter.Translate(request, rerankRoute()); err == nil {
			t.Errorf("%s: translated", name)
		}
	}
}
