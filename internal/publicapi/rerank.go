package publicapi

import (
	"net/http"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
)

/* -------------------------------------------------------------------------- */
/*  POST /v1/rerank                                                           */
/* -------------------------------------------------------------------------- */

// rerankRequest is rerankRequestSchema.
type rerankRequest struct {
	Model           contract.ModelReference `json:"model"`
	Query           string                  `json:"query"`
	Documents       []string                `json:"documents"`
	TopN            *int                    `json:"top_n,omitempty"`
	ReturnDocuments *bool                   `json:"return_documents,omitempty"`
	User            *string                 `json:"user,omitempty"`
}

// rerankDocument is a result's echoed document.
type rerankDocument struct {
	Text string `json:"text"`
}

// rerankResult is one element of rerankResponseSchema's results.
type rerankResult struct {
	Index          int             `json:"index"`
	RelevanceScore float64         `json:"relevance_score"`
	Document       *rerankDocument `json:"document,omitempty"`
}

// rerankResponse is rerankResponseSchema. It carries no amount.
type rerankResponse struct {
	ID      contract.GenerationID   `json:"id"`
	Model   contract.ModelReference `json:"model"`
	Results []rerankResult          `json:"results"`
	Usage   struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// estimateRerankInputTokens is the grant's estimatedInputTokens for a rerank:
// the same characters-to-tokens estimate embeddings use (a quarter of the
// UTF-16 length, plus one per piece), over the query and every document, which
// is the bound Oxy's planner holds (chars(query) + the sum of chars(documents)).
func estimateRerankInputTokens(query string, documents []string) int {
	estimate := utf16Length(query)/4 + 1
	for _, document := range documents {
		estimate += utf16Length(document)/4 + 1
	}
	return estimate
}

// handleRerank serves POST /v1/rerank. It validates the request strictly and
// refuses it before any grant unless some deployment of the model could rerank
// (servesRerank): a model nothing here serves holds no money. The query, the
// documents and their scores are customer content: never logged, and the
// request is sent to Oxy as neither text nor hash.
func (s *Server) handleRerank(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	var request rerankRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	switch {
	case !request.Model.Valid():
		failure = invalid(requestID, "model", "model is a model reference (publisher/model[@revision])")
	case request.Query == "":
		failure = invalid(requestID, "query", "query is not empty")
	case len(request.Documents) < 1 || len(request.Documents) > contract.MaxRerankDocuments:
		failure = invalid(requestID, "documents", "documents holds 1 to 1000 strings")
	case request.TopN != nil && *request.TopN < 1:
		failure = invalid(requestID, "top_n", "top_n is at least 1")
	case request.User != nil && (*request.User == "" || utf16Length(*request.User) > 64):
		failure = invalid(requestID, "user", "user is 1 to 64 characters")
	}
	if failure == nil && slices.Contains(request.Documents, "") {
		failure = invalid(requestID, "documents", "a document is not empty")
	}
	if failure == nil && !s.servesRerank(request.Model) {
		failure = contract.NewPublicError(requestID, "unsupported_modality", "no model is served for rerank").WithParam("model")
	}
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}

	estimate := estimateRerankInputTokens(request.Query, request.Documents)
	target := contract.ModelTarget(request.Model)
	ask := authorization{operation: contract.GrantRerank, target: &target, body: body, endpoint: "/v1/rerank",
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate}}
	if request.User != nil {
		user := contract.UserID(*request.User)
		ask.onBehalfOf = &user
	}
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, ask)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	envelope := admitted.request(contract.ModalityText, contract.APIFormatRerank, contract.Input{Format: contract.InputText, Text: &request.Query})
	envelope.Rerank = &contract.RerankParameters{Documents: request.Documents, TopN: request.TopN, ReturnDocuments: request.ReturnDocuments}
	result := s.execute(r.Context(), admitted, &envelope, func(contract.StreamEvent) error { return nil })
	if result.Failure != nil || result.Rerank == nil || len(result.Rerank.Results) == 0 || result.Report == nil || result.Report.GenerationID == nil {
		writeError(w, requestID, failureOf(result, requestID))
		return
	}
	echo := request.ReturnDocuments != nil && *request.ReturnDocuments
	answer := rerankResponse{ID: *result.Report.GenerationID, Model: result.Report.ResolvedModelReference, Results: make([]rerankResult, 0, len(result.Rerank.Results))}
	for _, ranked := range result.Rerank.Results {
		entry := rerankResult{Index: ranked.Index, RelevanceScore: ranked.RelevanceScore}
		if echo && ranked.Index >= 0 && ranked.Index < len(request.Documents) {
			entry.Document = &rerankDocument{Text: request.Documents[ranked.Index]}
		}
		answer.Results = append(answer.Results, entry)
	}
	// The provider's own count when it reported one; otherwise the estimate the
	// hold was sized on, which the usage report already marks as estimated.
	tokens := result.Rerank.InputTokens
	if tokens <= 0 {
		tokens = estimate
	}
	answer.Usage.PromptTokens, answer.Usage.TotalTokens = tokens, tokens
	writeJSON(w, requestID, http.StatusOK, answer)
}
