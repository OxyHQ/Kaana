package openaicompat

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Rerank on an OpenAI-compatible slug: `POST {base}/rerank`, the Cohere-style
// shape OpenRouter documents (model, query, documents, top_n; results with an
// index and a relevance score). Which slugs speak it is
// providerconfig.ExecutableAPIFormats' answer.
//
// Only fields the request needs are sent. `return_documents` is never sent: a
// ranked document's text is the caller's own, so the public handler fills it in
// from the request by index, and no provider's echo of customer text is relied
// on. Documents and the query are customer data: never logged or persisted.

const (
	rerankPath = "/rerank"
	// maxRerankResponseBytes bounds one buffered answer: 1000 scored documents,
	// even with each document echoed back, fit well inside it.
	maxRerankResponseBytes = 64 << 20
)

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      *int     `json:"top_n,omitempty"`
	// Provider is OpenRouter's routing policy and is sent to OpenRouter only.
	Provider *openRouterProviderPolicy `json:"provider,omitempty"`
}

// tokenCounts are the token figures providers put in a `usage` or
// `meta.tokens` / `meta.billed_units` object. `search_units` is deliberately
// absent: it is a count of searches, not tokens.
type tokenCounts struct {
	PromptTokens int `json:"prompt_tokens"`
	InputTokens  int `json:"input_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type rerankResponse struct {
	Results []struct {
		Index          *int     `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
	} `json:"results"`
	Usage *tokenCounts `json:"usage"`
	Meta  *struct {
		BilledUnits *tokenCounts `json:"billed_units"`
		Tokens      *tokenCounts `json:"tokens"`
	} `json:"meta"`
}

// reported is the input tokens the answer states, from the first place that
// states a positive count: usage, then meta.billed_units, then meta.tokens. A
// bare total is used only when nothing else is stated, as a rerank has no
// output tokens. Zero means the provider reported none.
func (r rerankResponse) reported() int {
	candidates := []*tokenCounts{r.Usage}
	if r.Meta != nil {
		candidates = append(candidates, r.Meta.BilledUnits, r.Meta.Tokens)
	}
	for _, counts := range candidates {
		if counts == nil {
			continue
		}
		if counts.PromptTokens > 0 {
			return counts.PromptTokens
		}
		if counts.InputTokens > 0 {
			return counts.InputTokens
		}
	}
	for _, counts := range candidates {
		if counts != nil && counts.TotalTokens > 0 {
			return counts.TotalTokens
		}
	}
	return 0
}

func (a *Adapter) translateRerank(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if !provider.Executes(a, contract.APIFormatRerank) || route.Provider != a.config.Provider {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment does not expose a rerank endpoint"}
	}
	if family, classified, _ := providerconfig.ClassifyModel(a.config.Provider, route.UpstreamModelID); classified && family.Format != contract.APIFormatRerank {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment's model does not rerank"}
	}
	switch {
	case request.Rerank == nil || request.Input.Format != contract.InputText || request.Input.Text == nil:
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input", Detail: "rerank takes a text query and documents"}
	case request.Stream:
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "stream", Detail: "rerank is non-streaming"}
	}
	body := rerankRequest{Model: route.UpstreamModelID, Query: *request.Input.Text, Documents: request.Rerank.Documents, TopN: request.Rerank.TopN}
	if a.config.Provider == "openrouter" {
		// The same non-negotiable policy every OpenRouter request carries:
		// zero data retention and no data collection.
		body.Provider = &openRouterProviderPolicy{ZDR: true, DataCollection: "deny", RequireParameters: true, Ignore: route.AvoidUpstreams}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding rerank request: %w", err)
	}
	headers := make(http.Header, len(a.config.Headers)+1)
	for name, value := range a.config.Headers {
		headers.Set(name, value)
	}
	headers.Set("Content-Type", "application/json")
	return &provider.Call{Route: route, Method: http.MethodPost, URL: a.config.BaseURL + rerankPath, Body: encoded, Header: headers}, nil
}

// isRerankCall reports a call translateRerank built.
func isRerankCall(call *provider.Call) bool {
	return !call.Stream && strings.HasSuffix(call.URL, rerankPath)
}

// noRanking is the failure of an attempt that answered 200 and scored nothing.
// It is the deployment's, so it is a server error the breaker, the retry and
// failover read like any other, and the customer is not billed for it.
func noRanking(slug contract.ProviderSlug) error {
	return provider.ErrUpstream{
		Code:     contract.CodeProviderError,
		Category: contract.UpstreamServerError,
		Detail:   fmt.Sprintf("%s completed the rerank request without scoring any document", slug),
	}
}

// readRerank decodes the answer against the request that was sent (documents
// is how many it held, topN the cap it asked for, 0 for none). A result whose
// index is outside the documents, repeated, or without a score is a provider
// error, never a ranking a caller's index could mis-resolve. The ranking is
// returned most relevant first and capped at topN whatever the provider did.
func (a *Adapter) readRerank(body io.Reader, documents, topN int) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported}
	var response rerankResponse
	if err := json.NewDecoder(io.LimitReader(body, maxRerankResponseBytes+1)).Decode(&response); err != nil {
		return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "unreadable rerank response"}
	}
	seen := make(map[int]struct{}, len(response.Results))
	ranked := make([]provider.RankedDocument, 0, len(response.Results))
	for _, result := range response.Results {
		switch {
		case result.Index == nil || result.RelevanceScore == nil || math.IsNaN(*result.RelevanceScore) || math.IsInf(*result.RelevanceScore, 0):
			return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "a rerank result has no index or score"}
		case *result.Index < 0 || *result.Index >= documents:
			return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "a rerank result indexes a document the request did not send"}
		}
		if _, repeated := seen[*result.Index]; repeated {
			return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "a rerank result repeats a document"}
		}
		seen[*result.Index] = struct{}{}
		ranked = append(ranked, provider.RankedDocument{Index: *result.Index, RelevanceScore: *result.RelevanceScore})
	}
	if len(ranked) == 0 {
		return outcome, noRanking(a.config.Provider)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].RelevanceScore > ranked[j].RelevanceScore })
	if topN > 0 && len(ranked) > topN {
		ranked = ranked[:topN]
	}
	tokens := response.reported()
	outcome.Rerank = &provider.RerankResult{Results: ranked, InputTokens: tokens}
	outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
	if tokens > 0 {
		outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: tokens})
	} else {
		// Nothing was reported: the request is metered as one request and the
		// caller's estimate stands, so the source says it is not measured.
		outcome.UsageSource = contract.UsageEstimated
	}
	outcome.FinishReason = contract.FinishStop
	return outcome, nil
}
