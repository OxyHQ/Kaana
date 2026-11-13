package openaicompat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/conformance"
)

const rerankUpstreamModel = "cohere/rerank-v3.5"

// OpenRouter passes the rerank suite against a fake of its documented wire.
func TestRerankAdapterConforms(t *testing.T) {
	subject := conformance.RerankSubject{
		Name: "openrouter-rerank", Provider: "openrouter",
		ModelReference: "cohere/rerank-v3.5@2026-01-01", UpstreamModelID: rerankUpstreamModel,
		Documents: 4, TopN: 2, InputTokens: 37,
		NewAdapter: func(t *testing.T, upstreamURL string) provider.Adapter {
			return embeddingAdapter(t, "openrouter", upstreamURL)
		},
		StartUpstream: startRerankFake,
	}
	conformance.RunRerank(t, subject)
}

func startRerankFake(t *testing.T, scenario conformance.RerankScenario) *conformance.RerankUpstream {
	t.Helper()
	var mutex sync.Mutex
	requests, disconnected := 0, false
	arrived := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mutex.Lock()
		requests++
		mutex.Unlock()
		decoded := struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}{}
		_ = json.Unmarshal(raw, &decoded)
		if !strings.HasSuffix(r.URL.Path, "/rerank") || decoded.Model != rerankUpstreamModel || decoded.Query != conformance.RerankQuery ||
			len(decoded.Documents) != 4 || decoded.Documents[3] != conformance.RerankDocument(3) || decoded.TopN != 2 {
			t.Errorf("the upstream received %s %s", r.URL.Path, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		switch scenario {
		case conformance.RerankAnswered:
			// Deliberately unsorted and longer than top_n: the adapter orders
			// and caps, whatever the provider did.
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"gen-1","model":%q,"results":[{"index":1,"relevance_score":0.2},{"index":3,"relevance_score":0.9},{"index":0,"relevance_score":0.5}],"usage":{"prompt_tokens":37,"total_tokens":37}}`, rerankUpstreamModel))
		case conformance.RerankEmpty:
			_, _ = io.WriteString(w, `{"model":"x","results":[],"usage":{"prompt_tokens":37,"total_tokens":37}}`)
		case conformance.RerankRateLimited:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached","type":"rate_limit_exceeded"}}`)
		case conformance.RerankServerError:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"The server had an error","type":"server_error"}}`)
		case conformance.RerankRequestRefused:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid value for 'top_n'","type":"invalid_request_error","param":"top_n"}}`)
		case conformance.RerankHeld:
			once.Do(func() { close(arrived) })
			<-r.Context().Done()
			mutex.Lock()
			disconnected = true
			mutex.Unlock()
		}
	}))
	t.Cleanup(server.Close)
	return &conformance.RerankUpstream{
		URL:          server.URL,
		Requests:     func() int { mutex.Lock(); defer mutex.Unlock(); return requests },
		Disconnected: func() bool { mutex.Lock(); defer mutex.Unlock(); return disconnected },
		Arrived:      arrived,
	}
}
