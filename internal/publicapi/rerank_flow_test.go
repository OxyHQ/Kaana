package publicapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// rerankStubAdapter is a rerank provider the test scripts. It records every
// envelope it is handed and ranks the documents in reverse order.
type rerankStubAdapter struct {
	pool *provider.KeyPool
	mu   sync.Mutex
	// mode: "" serves, "empty" answers with no scored document, "silent"
	// serves and reports no token count.
	mode  string
	calls []*contract.Request
}

func (*rerankStubAdapter) Provider() contract.ProviderSlug { return "stubrerank" }
func (s *rerankStubAdapter) PlatformCredentials() *provider.KeyPool {
	return s.pool
}
func (*rerankStubAdapter) APIFormats() []contract.APIFormat {
	return []contract.APIFormat{contract.APIFormatRerank}
}
func (*rerankStubAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: "stubrerank", Status: provider.HealthOK, CheckedAt: contract.NewTimestamp(time.Now())}
}

func (s *rerankStubAdapter) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *rerankStubAdapter) called() []*contract.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*contract.Request(nil), s.calls...)
}

func (s *rerankStubAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	s.mu.Lock()
	copied := *request
	s.calls = append(s.calls, &copied)
	s.mu.Unlock()
	return &provider.Call{Route: route, Method: http.MethodPost, URL: "stub://rerank"}, nil
}

func (s *rerankStubAdapter) Stream(_ context.Context, _ *provider.Call, _ provider.Emitter, _ *provider.KeyPool) (provider.Outcome, error) {
	s.mu.Lock()
	mode, request := s.mode, s.calls[len(s.calls)-1]
	s.mu.Unlock()
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}
	if mode == "empty" {
		outcome.Rerank = &provider.RerankResult{}
		outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 9}}
		return outcome, nil
	}
	documents := len(request.Rerank.Documents)
	result := &provider.RerankResult{InputTokens: 11}
	for index := documents - 1; index >= 0; index-- {
		result.Results = append(result.Results, provider.RankedDocument{Index: index, RelevanceScore: float64(index+1) / 10})
	}
	outcome.Rerank = result
	outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 11}}
	if mode == "silent" {
		result.InputTokens = 0
		outcome.UsageSource = contract.UsageEstimated
		outcome.Units = outcome.Units[:1]
	}
	return outcome, nil
}

func rerankBody(extra map[string]any) map[string]any {
	body := map[string]any{"model": string(rerankModel), "query": "SECRET-QUERY", "documents": []string{"SECRET-DOC-A", "SECRET-DOC-B", "SECRET-DOC-C"}}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

// A served rerank returns the contract's shape with the documents echoed from
// the request, tells Oxy the estimated input tokens, hands the adapter the
// query, documents and controls, and settles requests plus input tokens.
func TestARerankRequestIsServedAndSettled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/rerank", validKey, rerankBody(map[string]any{"top_n": 2, "return_documents": true, "user": "end-user-1"}))
	answer := readAll(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, answer)
	}
	var decoded struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Results []struct {
			Index    int                   `json:"index"`
			Score    float64               `json:"relevance_score"`
			Document struct{ Text string } `json:"document"`
		} `json:"results"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(answer, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID == "" || decoded.Model != string(rerankModel) || len(decoded.Results) != 3 || decoded.Results[0].Index != 2 || decoded.Results[0].Document.Text != "SECRET-DOC-C" {
		t.Errorf("answer = %s", answer)
	}
	if decoded.Usage["prompt_tokens"] != 11 || decoded.Usage["total_tokens"] != 11 || len(decoded.Usage) != 2 {
		t.Errorf("usage = %v; the provider's count, and nothing else", decoded.Usage)
	}
	if strings.Contains(string(answer), "cost") || strings.Contains(string(answer), "amount") {
		t.Errorf("answer carries an amount: %s", answer)
	}

	var asked contract.InferenceAuthorizationRequest
	authorization := h.oxy.authorizationBodies()[0]
	if err := json.Unmarshal([]byte(authorization), &asked); err != nil {
		t.Fatal(err)
	}
	// "SECRET-QUERY" (12 chars) and three 12-char documents: 4 pieces of 12/4+1.
	if asked.Operation != contract.GrantRerank || asked.Facts.EstimatedInputTokens == nil || *asked.Facts.EstimatedInputTokens != 16 {
		t.Errorf("authorization = %+v", asked)
	}
	if strings.Contains(authorization, "SECRET") {
		t.Error("customer text reached Oxy")
	}

	calls := h.rerank.called()
	if len(calls) != 1 || calls[0].Rerank == nil || *calls[0].Input.Text != "SECRET-QUERY" || len(calls[0].Rerank.Documents) != 3 || *calls[0].Rerank.TopN != 2 || calls[0].Client.APIFormat != contract.APIFormatRerank {
		t.Fatalf("the adapter was handed %+v", calls)
	}
	if err := calls[0].Validate(); err != nil {
		t.Errorf("the envelope handed to the adapter is invalid: %v", err)
	}

	h.settle()
	settled := h.oxy.settled()
	if len(settled) != 1 {
		t.Fatalf("%d settlements", len(settled))
	}
	for _, settlement := range settled {
		units := map[contract.UsageUnit]int{}
		for _, unit := range settlement.Report.Units {
			units[unit.Unit] = unit.Quantity
		}
		if settlement.Report.Outcome != contract.OutcomeCompleted || units[contract.UnitRequests] != 1 || units[contract.UnitInputTokens] != 11 {
			t.Errorf("settlement usage = %+v", settlement.Report)
		}
	}
	if strings.Contains(h.logs.String(), "SECRET") {
		t.Error("customer content reached the logs")
	}
}

// Documents are echoed only when asked, and a provider that reports no tokens
// leaves the estimate in the response and an estimated report.
func TestARerankWithoutEchoOrProviderUsage(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.rerank.setMode("silent")
	response := h.post("/v1/rerank", validKey, rerankBody(nil))
	answer := readAll(t, response)
	if response.StatusCode != http.StatusOK || strings.Contains(string(answer), "document") || strings.Contains(string(answer), "SECRET") {
		t.Fatalf("%d %s", response.StatusCode, answer)
	}
	if !strings.Contains(string(answer), `"prompt_tokens":16`) {
		t.Errorf("answer = %s; want the estimate of 16", answer)
	}
	h.settle()
	for _, settlement := range h.oxy.settled() {
		if settlement.Report.UsageSource != contract.UsageEstimated || len(settlement.Report.Units) != 1 || settlement.Report.Units[0].Unit != contract.UnitRequests {
			t.Errorf("settlement usage = %+v", settlement.Report)
		}
	}
}

// An attempt that scored nothing is never served or billed.
func TestAnEmptyRerankAnswerIsNeverServed(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.rerank.setMode("empty")
	failure := publicError(t, h.post("/v1/rerank", validKey, rerankBody(nil)), http.StatusBadGateway)
	if failure.Code != "provider_error" {
		t.Errorf("failure = %+v", failure)
	}
	h.settle()
	for _, settlement := range h.oxy.settled() {
		if settlement.Report.Outcome != contract.OutcomeFailed || len(settlement.Report.Units) != 0 {
			t.Errorf("settlement usage = %+v; an empty attempt bills nothing", settlement.Report)
		}
	}
}

// A model with no deployment that can rerank is refused before Oxy is asked.
func TestAModelNothingServesForRerankIsRefusedBeforeAGrant(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for _, model := range []string{"stub/model", "stub/model@2026-05-01", "stub/unknown", string(rerankModel) + "9", string(imageModel)} {
		failure := publicError(t, h.post("/v1/rerank", validKey, rerankBody(map[string]any{"model": model})), http.StatusBadRequest)
		if failure.Code != "unsupported_modality" || failure.Param == nil || *failure.Param != "model" {
			t.Errorf("%s: failure = %+v", model, failure)
		}
	}
	if len(h.oxy.authorizationBodies()) != 0 || len(h.rerank.called()) != 0 {
		t.Errorf("%d authorizations, %d adapter calls; a refused request holds nothing", len(h.oxy.authorizationBodies()), len(h.rerank.called()))
	}
	if response := h.post("/v1/rerank", validKey, rerankBody(nil)); response.StatusCode != http.StatusOK {
		t.Fatalf("the rerank model was refused: %d", response.StatusCode)
	}
}

func TestRerankRequestValidation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	many := make([]string, 1001)
	for index := range many {
		many[index] = "d"
	}
	for name, extra := range map[string]map[string]any{
		"query":     {"query": ""},
		"documents": {"documents": []string{}},
		"top_n":     {"top_n": 0},
		"user":      {"user": ""},
		"model":     {"model": "not a model"},
	} {
		failure := publicError(t, h.post("/v1/rerank", validKey, rerankBody(extra)), http.StatusBadRequest)
		if failure.Code != "invalid_request" || failure.Param == nil || *failure.Param != name {
			t.Errorf("%s: failure = %+v", name, failure)
		}
	}
	for name, extra := range map[string]map[string]any{
		"too many documents": {"documents": many},
		"empty document":     {"documents": []string{"a", ""}},
		"unknown field":      {"stream": true},
		"object document":    {"documents": []any{map[string]string{"text": "a"}}},
	} {
		failure := publicError(t, h.post("/v1/rerank", validKey, rerankBody(extra)), http.StatusBadRequest)
		if failure.Code != "invalid_request" {
			t.Errorf("%s: failure = %+v", name, failure)
		}
	}
	if len(h.oxy.authorizationBodies()) != 0 {
		t.Error("an invalid request asked Oxy for a grant")
	}
}
