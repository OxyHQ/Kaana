package openaicompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/scopedpermit"
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

// defaultDecisionDeadline bounds one decisions request upstream.
const defaultDecisionDeadline = 30 * time.Second

func (a *Adapter) decisionDeadline() time.Duration {
	if a.decisionTimeout > 0 {
		return a.decisionTimeout
	}
	return defaultDecisionDeadline
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

type systemOneRequest struct {
	Model     string                                   `json:"model"`
	State     string                                   `json:"state"`
	Questions map[string]contract.DecisionWireQuestion `json:"questions"`
	Provider  *decisionProviderPolicy                  `json:"provider,omitempty"`
}

func (a *Adapter) translateDecisions(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if err := request.ValidateDecisionsEnvelope(); err != nil {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input", Detail: "invalid typed decisions envelope"}
	}
	if child := request.PrivateAutoExecution; child != nil {
		if request.ValidatePrivateAutoExecution() != nil || a.privateAutoSource == nil || !child.MatchesSource(a.privateAutoSource(), time.Now()) || !route.PrivateAutoSourceApproval.Equal(a.privateAutoSource()) || route.ScopedExecution != nil || route.ScopedDecisionPriceLimit == nil || route.ScopedDecisionPriceLimit.Prompt.IsZero() || !route.ScopedDecisionPriceLimit.Completion.IsZero() {
			return nil, decisionsUnavailable()
		}
	} else if request.ScopedExecution != nil {
		if err := request.ValidateScopedExecution(); err != nil {
			return nil, decisionsUnavailable()
		}
		scope := &request.ScopedExecution.ScopedExecutionAudience
		if a.scopedSource == nil || !scopedpermit.Matches(scope, a.scopedSource(), time.Now()) || !scope.Equal(route.ScopedExecution) || route.ScopedDecisionPriceLimit == nil || route.ScopedDecisionPriceLimit.Prompt.IsZero() || !route.ScopedDecisionPriceLimit.Completion.IsZero() {
			return nil, decisionsUnavailable()
		}
	} else if !a.decisions.approved() || route.ScopedExecution != nil || route.PrivateAutoSourceApproval != nil {
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
	// The body is built from the contract's own wire representation, so what is
	// measured below is byte for byte what is sent.
	body := systemOneRequest{Model: route.UpstreamModelID, State: input.State, Questions: input.DecisionWireQuestions(input.Questions)}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("systemone: encode request: %w", err)
	}
	tooLarge := provider.ErrUnsupported{Code: contract.CodeRequestTooLarge, Param: "input.decisions", Detail: "the serialized systemone body exceeds its input budget"}
	if len(encoded) > contract.DecisionTotalBudget {
		return nil, tooLarge
	}
	for i := range input.Questions {
		single, err := json.Marshal(systemOneRequest{Model: route.UpstreamModelID, State: input.State, Questions: input.DecisionWireQuestions(input.Questions[i : i+1])})
		if err != nil {
			return nil, fmt.Errorf("systemone: encode request: %w", err)
		}
		if len(single) > contract.DecisionContextBudget {
			return nil, tooLarge
		}
	}
	if a.Provider() == "openrouter" {
		// A gateway's limit is on its whole body. The policy must fit inside the
		// allowance Oxy reserved for it, and the questions with that allowance
		// must fit inside the gateway's total.
		core := len(encoded)
		body.Provider = &decisionProviderPolicy{Only: []string{"TypeSafe"}, Order: []string{"TypeSafe"}, Ignore: []string{}, DataCollection: "deny", RequireParameters: true, ZDR: true}
		if request.ScopedExecution != nil || request.PrivateAutoExecution != nil {
			body.Provider.MaxPrice = *route.ScopedDecisionPriceLimit
		}
		if encoded, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("systemone: encode request: %w", err)
		}
		// Oxy's helper bounds the input with a 255-byte model allowance; the
		// final body is bounded again as sent, so neither can stand in for the
		// other if the wire shape or the policy grows.
		if !input.FitsGateway() || !gatewayBodyFits(core, len(encoded)) {
			return nil, tooLarge
		}
	}
	if child := request.PrivateAutoExecution; child != nil {
		if route.ScopedDecisionPriceLimit == nil || len(encoded) > contract.PrivateAutoMaximumBytes || !providercost.PrivateDecisionQuoteWithin(*route.ScopedDecisionPriceLimit, len(encoded), child.MaxCostUSD) {
			return nil, provider.ErrUnsupported{Code: contract.CodePermissionDenied, Detail: "private Auto actual quote exceeds source ceiling"}
		}
	}
	return &provider.Call{PrivateAutoExecution: request.PrivateAutoExecution, RequestID: request.Attribution.RequestID, Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/systemone", Body: encoded, Header: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}, Decisions: input}, nil
}

// gatewayBodyFits bounds a gateway body as sent: the questions plus the
// reserved allowance fit the gateway's total, and the policy fits inside that
// allowance.
func gatewayBodyFits(core, final int) bool {
	return core+contract.DecisionGatewayAllowance <= contract.DecisionGatewayBudget &&
		final <= core+contract.DecisionGatewayAllowance
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
	ambiguity := unambiguousDecisionJSON(json.NewDecoder(bytes.NewReader(raw)), systemOneResponseShape)
	// Account for reported work even when answer identity or money is malformed,
	// but never from a usage object the decoder could have read two ways.
	if !errors.Is(ambiguity, errAmbiguousUsage) {
		if response.Usage.InputTokens != nil && *response.Usage.InputTokens >= 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: *response.Usage.InputTokens})
		}
		if response.Usage.OutputTokens != nil && *response.Usage.OutputTokens >= 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: *response.Usage.OutputTokens})
		}
		// Unknown (nil) when absent or not exactly representable: never zero,
		// never rounded, and never a reason to discard a paid answer.
		outcome.ProviderReportedCost = response.Usage.ReportedCost()
	}
	outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitRequests, Quantity: 1})
	if ambiguity != nil {
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

// jsonShape says how Go's decoder reads one JSON object. A struct matches
// member names case-insensitively (Unicode simple folding), so "model" and
// "MODEL" are one field and the later silently wins; a map keeps exact keys.
type jsonShape struct {
	fields map[string]*jsonShape // a struct: its exact member names
	values *jsonShape            // a map: the shape of every value
	usage  bool                  // ambiguity here makes measured units unreliable
}

var systemOneResponseShape = &jsonShape{fields: map[string]*jsonShape{
	"model": nil, "id": nil, "provider": nil,
	"usage": {usage: true, fields: map[string]*jsonShape{"input_tokens": nil, "output_tokens": nil, "cost": nil}},
	"answers": {values: &jsonShape{fields: map[string]*jsonShape{
		"type": nil, "choice": nil, "confidence": nil, "score": nil, "noul": nil,
		"probabilities": {values: nil},
	}}},
}}

var (
	errAmbiguousJSON  = errors.New("ambiguous systemone member")
	errAmbiguousUsage = fmt.Errorf("%w in usage", errAmbiguousJSON)
)

// unambiguousDecisionJSON rejects any object the decoder could read two ways:
// an exact duplicate anywhere, and in a struct-decoded object a member spelled
// other than exactly, or two members that fold to one name. Duplicates make
// answer identity and probability cardinality ambiguous. An ambiguity inside
// usage is reported as errAmbiguousUsage, because then no counter is reliable.
func unambiguousDecisionJSON(decoder *json.Decoder, shape *jsonShape) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	inUsage := shape != nil && shape.usage
	seen := map[string]bool{}
	for decoder.More() {
		var child *jsonShape
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, _ := key.(string)
			identity, exact := name, true
			switch {
			case shape != nil && shape.fields != nil:
				identity = foldJSONName(name)
				for field, fieldShape := range shape.fields {
					if foldJSONName(field) == identity {
						child, exact = fieldShape, field == name
					}
				}
			case shape != nil:
				child = shape.values
			}
			if !exact || seen[identity] {
				if inUsage || (child != nil && child.usage) {
					return errAmbiguousUsage
				}
				return errAmbiguousJSON
			}
			seen[identity] = true
		}
		if err := unambiguousDecisionJSON(decoder, child); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// foldJSONName is encoding/json's foldName: names are one field exactly when
// bytes.EqualFold says so.
func foldJSONName(name string) string {
	var folded strings.Builder
	for _, r := range name {
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				r = next
				break
			}
			r = next
		}
		folded.WriteRune(r)
	}
	return folded.String()
}
