package openaicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// These independent reviews have no production configuration switch. Only
// synthetic conformance fixtures can affirm them in this package. Ordinary
// credentials confer none of these rights; see docs/decisions.md.
type decisionReview struct {
	resale, internalEligibility, privacy, zdr, immutableRoute bool
}

func (r decisionReview) approved() bool {
	return r.resale && r.internalEligibility && r.privacy && r.zdr && r.immutableRoute
}

func decisionsUnavailable() error {
	return provider.ErrUnsupported{Code: contract.CodePermissionDenied, Param: "client.apiFormat", Detail: "decisions are dormant pending independent rights, eligibility, privacy, ZDR and immutable route review"}
}

type decisionProviderPolicy struct {
	Only              []string                        `json:"only"`
	Order             []string                        `json:"order"`
	Ignore            []string                        `json:"ignore"`
	AllowFallbacks    bool                            `json:"allow_fallbacks"`
	DataCollection    string                          `json:"data_collection"`
	RequireParameters bool                            `json:"require_parameters"`
	MaxPrice          providercost.DecisionPriceLimit `json:"max_price"`
	ZDR               bool                            `json:"zdr"`
}

type systemOneQuestion struct {
	Type         contract.DecisionKind `json:"type"`
	Instructions any                   `json:"instructions"`
	Criteria     any                   `json:"criteria,omitempty"`
}

type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     string                       `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
	Provider  *decisionProviderPolicy      `json:"provider,omitempty"`
}

func (a *Adapter) translateDecisions(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if err := request.ValidateDecisionsEnvelope(); err != nil {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input", Detail: "invalid typed decisions envelope"}
	}
	if !a.decisions.approved() {
		return nil, decisionsUnavailable()
	}
	if route.Provider != a.Provider() || !route.ModelReference.Valid() || !route.ModelReference.Pinned() || *request.Target.ModelReference != route.ModelReference {
		return nil, decisionsUnavailable()
	}
	switch a.Provider() {
	case "typesafe":
		if route.UpstreamModelID != "jev-1.13.0" {
			return nil, decisionsUnavailable()
		}
	case "openrouter":
		// This candidate is deliberately not in production inventory. A returned
		// dated id alone does not prove it is accepted as an immutable request id.
		if route.UpstreamModelID != "typesafe/jev-1.13-20260917" {
			return nil, decisionsUnavailable()
		}
	default:
		return nil, decisionsUnavailable()
	}
	input := request.Input.Decisions
	if input.Effort != nil {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input.decisions.effort", Detail: "the systemone API has no verified effort field"}
	}
	body := systemOneRequest{Model: route.UpstreamModelID, State: input.State, Questions: make(map[string]systemOneQuestion, len(input.Questions))}
	translatedBytes := len(input.State)
	for _, q := range input.Questions {
		// The provider documents structured instructions. Keep common instructions,
		// the question and its rubric distinct, without inventing prompt delimiters.
		translatedBytes += len(q.ID) + len(q.Question)
		for _, label := range append(append([]string{}, q.Options...), q.Levels...) {
			translatedBytes += len(label)
		}
		instructions := map[string]string{"question": q.Question}
		if input.Instructions != nil {
			instructions["instructions"] = *input.Instructions
			translatedBytes += len(*input.Instructions)
		}
		if q.Criteria != nil {
			instructions["criteria"] = *q.Criteria
			translatedBytes += len(*q.Criteria)
		}
		wire := systemOneQuestion{Type: q.Kind, Instructions: instructions}
		switch q.Kind {
		case "choice":
			labels := make(map[string]any, len(q.Options))
			for _, label := range q.Options {
				labels[label] = nil
			}
			wire.Criteria = labels
		case "score":
			wire.Criteria = q.Levels
		}
		body.Questions[q.ID] = wire
	}
	if translatedBytes > 65536 {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input.decisions.instructions", Detail: "per-question instructions exceed the systemone total input budget"}
	}
	if a.Provider() == "openrouter" {
		body.Provider = &decisionProviderPolicy{Only: []string{"TypeSafe"}, Order: []string{"TypeSafe"}, Ignore: []string{}, DataCollection: "deny", RequireParameters: true, ZDR: true}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("systemone: encode request: %w", err)
	}
	return &provider.Call{RequestID: request.Attribution.RequestID, Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/systemone", Body: encoded, Header: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}, Decisions: input}, nil
}

type systemOneAnswer struct {
	Choice        *string               `json:"choice"`
	Confidence    *float64              `json:"confidence"`
	Type          contract.DecisionKind `json:"type"`
	Probabilities map[string]*float64   `json:"probabilities"`
	Score         *float64              `json:"score"`
	Noul          *float64              `json:"noul"`
}

type systemOneResponse struct {
	Model    string                     `json:"model"`
	Answers  map[string]json.RawMessage `json:"answers"`
	Usage    providercost.DecisionUsage `json:"usage"`
	ID       string                     `json:"id"`
	Provider string                     `json:"provider"`
}

func (a *Adapter) readDecisions(body io.Reader, call *provider.Call) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported}
	fail := func() (provider.Outcome, error) {
		return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "invalid systemone response"}
	}
	var response systemOneResponse
	raw, readErr := io.ReadAll(io.LimitReader(body, (2<<20)+1))
	if readErr != nil || len(raw) > 2<<20 {
		return fail()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&response); err != nil {
		return fail()
	}
	// Account for reported work even when answer identity or money is malformed.
	if response.Usage.InputTokens != nil && *response.Usage.InputTokens >= 0 {
		outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: *response.Usage.InputTokens})
	}
	if response.Usage.OutputTokens != nil && *response.Usage.OutputTokens >= 0 {
		outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: *response.Usage.OutputTokens})
	}
	outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitRequests, Quantity: 1})
	cost, err := response.Usage.ReportedCost()
	if err != nil {
		return fail()
	}
	outcome.ProviderReportedCost = cost
	if err := uniqueDecisionJSON(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return fail()
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fail()
	}
	if call.Decisions == nil || response.Model != call.Route.UpstreamModelID || response.Usage.InputTokens == nil || response.Usage.OutputTokens == nil || *response.Usage.InputTokens < 0 || *response.Usage.OutputTokens < 0 || len(response.Answers) != len(call.Decisions.Questions) {
		return fail()
	}
	if a.Provider() == "openrouter" && (response.Provider != "TypeSafe" || response.ID == "") {
		return fail()
	}
	answers := make([]contract.DecisionAnswer, 0, len(response.Answers))
	for _, question := range call.Decisions.Questions {
		rawAnswer, ok := response.Answers[question.ID]
		var wire systemOneAnswer
		if !ok || json.Unmarshal(rawAnswer, &wire) != nil || wire.Type != question.Kind {
			return fail()
		}
		answer := contract.DecisionAnswer{ID: question.ID, Kind: question.Kind}
		switch question.Kind {
		case "choice":
			answer.Reply = &contract.DecisionReply{Label: wire.Choice}
			answer.Confidence = wire.Confidence
			if len(wire.Probabilities) != len(question.Options) {
				return fail()
			}
			for _, option := range question.Options {
				p, ok := wire.Probabilities[option]
				if !ok || p == nil {
					return fail()
				}
				answer.Probabilities = append(answer.Probabilities, *p)
			}
		case "score":
			if len(wire.Probabilities) != len(question.Levels) {
				return fail()
			}
			for i := range question.Levels {
				p, ok := wire.Probabilities[strconv.Itoa(i)]
				if !ok || p == nil {
					return fail()
				}
				answer.Distribution = append(answer.Distribution, *p)
			}
			answer.Mean = wire.Score
			answer.Reply = &contract.DecisionReply{Score: wire.Score}
			answer.Confidence = wire.Confidence
		case "noul":
			if wire.Confidence != nil || wire.Choice != nil || wire.Score != nil || wire.Probabilities != nil {
				return fail()
			}
			answer.Probability = wire.Noul
		}
		answers = append(answers, answer)
	}
	if err := contract.ValidateDecisionAnswers(*call.Decisions, answers); err != nil {
		return fail()
	}
	outcome.Decisions = answers
	outcome.FinishReason = contract.FinishStop
	return outcome, nil
}

// Duplicate JSON members make answer identity and probability cardinality
// ambiguous. Reject them, including nested maps, before accepting any answer.
func uniqueDecisionJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate systemone member")
			}
			seen[name] = true
		}
		if err := uniqueDecisionJSON(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
