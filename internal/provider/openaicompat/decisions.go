package openaicompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// defaultDecisionDeadline bounds one decisions request upstream.
const defaultDecisionDeadline = 30 * time.Second

func (a *Adapter) decisionDeadline() time.Duration {
	if a.decisionTimeout > 0 {
		return a.decisionTimeout
	}
	return defaultDecisionDeadline
}

// errNoDecisionPrice refuses a decisions call whose route carries no price
// ceiling: every decisions route is priced from its rate card, and one without
// a card is refused, never priced at zero (providercost.Cards.DecisionPriceLimit).
var errNoDecisionPrice = provider.ErrUnsupported{Code: contract.CodePermissionDenied, Param: "client.apiFormat", Detail: "this decisions route has no reviewed price"}

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

// translateDecisions builds the one upstream call of a decisions request:
// OpenRouter's `/systemone` for TypeSafe's Jev, or OpenAI's `/decisions`
// (decisions_openai.go). Every refusal is made here, before any spend.
func (a *Adapter) translateDecisions(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if err := request.ValidateDecisionsEnvelope(); err != nil {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input", Detail: "invalid typed decisions envelope"}
	}
	if route.DecisionPriceLimit == nil {
		return nil, errNoDecisionPrice
	}
	target := request.Target.ModelReference
	pinnedElsewhere := request.Target.Kind == contract.TargetModel && target.Pinned() && *target != route.ModelReference
	if route.Provider != a.Provider() || !route.ModelReference.Pinned() || pinnedElsewhere {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "authorizedRoutes", Detail: "the route does not serve the pinned decisions model"}
	}
	input := request.Input.Decisions
	if input.Effort != nil {
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input.decisions.effort", Detail: "the decisions model takes no effort"}
	}
	switch {
	case a.Provider() == "openai-decisions":
		return a.translateOpenAIDecisions(request, route)
	case a.Provider() == "openrouter" && strings.HasPrefix(route.UpstreamModelID, "typesafe/"):
		return a.translateSystemOne(request, route)
	}
	return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "client.apiFormat", Detail: "this deployment does not answer decisions"}
}

var errDecisionsTooLarge = provider.ErrUnsupported{Code: contract.CodeRequestTooLarge, Param: "input.decisions", Detail: "the serialized decisions body exceeds its input budget"}

// encodeDecisionsBody encodes a decisions body and bounds it as sent by the
// contract's budgets: the whole body, and the body of each question alone.
func encodeDecisionsBody(questions []contract.DecisionQuestion, encode func([]contract.DecisionQuestion) ([]byte, error)) ([]byte, error) {
	encoded, err := encode(questions)
	if err != nil {
		return nil, fmt.Errorf("decisions: encode request: %w", err)
	}
	if len(encoded) > contract.DecisionTotalBudget {
		return nil, errDecisionsTooLarge
	}
	for i := range questions {
		single, err := encode(questions[i : i+1])
		if err != nil {
			return nil, fmt.Errorf("decisions: encode request: %w", err)
		}
		if len(single) > contract.DecisionContextBudget {
			return nil, errDecisionsTooLarge
		}
	}
	return encoded, nil
}

// translateSystemOne is OpenRouter's `/systemone`, which only TypeSafe's Jev
// answers, under a wire policy that pins TypeSafe, forbids fallbacks and
// requires zero data retention, with the route's price as `max_price`.
func (a *Adapter) translateSystemOne(request *contract.Request, route provider.Route) (*provider.Call, error) {
	input := request.Input.Decisions
	// The body is built from the contract's own wire representation, so what is
	// measured is byte for byte what is sent.
	encoded, err := encodeDecisionsBody(input.Questions, func(questions []contract.DecisionQuestion) ([]byte, error) {
		return json.Marshal(systemOneRequest{Model: route.UpstreamModelID, State: input.State, Questions: input.DecisionWireQuestions(questions)})
	})
	if err != nil {
		return nil, err
	}
	// A gateway's limit is on its whole body. The policy must fit inside the
	// allowance Oxy reserved for it, and the questions with that allowance
	// must fit inside the gateway's total.
	core := len(encoded)
	body := systemOneRequest{Model: route.UpstreamModelID, State: input.State, Questions: input.DecisionWireQuestions(input.Questions),
		Provider: &decisionProviderPolicy{Only: []string{"TypeSafe"}, Order: []string{"TypeSafe"}, Ignore: []string{}, DataCollection: "deny", RequireParameters: true, ZDR: true, MaxPrice: *route.DecisionPriceLimit}}
	if encoded, err = json.Marshal(body); err != nil {
		return nil, fmt.Errorf("decisions: encode request: %w", err)
	}
	// Oxy's helper bounds the input with a 255-byte model allowance; the
	// final body is bounded again as sent, so neither can stand in for the
	// other if the wire shape or the policy grows.
	if !input.FitsGateway() || !gatewayBodyFits(core, len(encoded)) {
		return nil, errDecisionsTooLarge
	}
	return &provider.Call{RequestID: request.Attribution.RequestID, Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/systemone", Body: encoded, Header: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}, Decisions: input}, nil
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

// readDecisions reads one decisions answer: the provider's own reading
// (readSystemOne, readOpenAIDecisions), then the contract's validation of
// every answer against the request. Measured units survive a refused answer.
func (a *Adapter) readDecisions(body io.Reader, call *provider.Call) (provider.Outcome, error) {
	read := readSystemOne
	if a.Provider() == "openai-decisions" {
		read = readOpenAIDecisions
	}
	outcome, answers, err := read(body, call)
	if err != nil {
		return outcome, err
	}
	if err := contract.ValidateDecisionAnswers(*call.Decisions, answers); err != nil {
		var invalid contract.DecisionAnswerError
		errors.As(err, &invalid)
		return outcome, provider.InvalidDecisionResponse(string(invalid))
	}
	outcome.Decisions = answers
	outcome.FinishReason = contract.FinishStop
	return outcome, nil
}

// decisionResponseFacts is what a decisions response says about itself: the
// model that answered and the work it measured.
type decisionResponseFacts struct {
	model         string
	input, output *int
	cost          *providercost.Money
}

const maxDecisionResponseBytes = 2 << 20

// decodeDecisionResponse reads one decisions response into response under the
// rules every decisions adapter shares: at most 2 MiB, one JSON value that the
// decoder cannot read two ways, from the model the route names. The work the
// provider measured is kept even when its answer is then refused, never from a
// usage object that could be read two ways. reason is empty when the body is
// usable.
func decodeDecisionResponse(body io.Reader, call *provider.Call, shape *jsonShape, response any, facts func() decisionResponseFacts) (outcome provider.Outcome, reason string) {
	outcome = provider.Outcome{UsageSource: contract.UsageProviderReported}
	raw, err := io.ReadAll(io.LimitReader(body, maxDecisionResponseBytes+1))
	if err != nil || len(raw) > maxDecisionResponseBytes {
		return outcome, "body_read"
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(response); err != nil {
		return outcome, "response_json"
	}
	ambiguity := unambiguousDecisionJSON(json.NewDecoder(bytes.NewReader(raw)), shape)
	stated := facts()
	if !errors.Is(ambiguity, errAmbiguousUsage) {
		if stated.input != nil && *stated.input >= 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: *stated.input})
		}
		if stated.output != nil && *stated.output >= 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: *stated.output})
		}
		// Unknown (nil) when absent or not exactly representable: never zero,
		// never rounded, and never a reason to discard a paid answer.
		outcome.ProviderReportedCost = stated.cost
	}
	outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitRequests, Quantity: 1})
	if ambiguity != nil {
		return outcome, "ambiguous_json"
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return outcome, "trailing_json"
	}
	if call.Decisions == nil {
		return outcome, "missing_questions"
	}
	if stated.model != call.Route.UpstreamModelID {
		return outcome, "model_identity"
	}
	return outcome, ""
}

// normalizeDistribution checks that every entry is a probability and that
// the mass is one within tolerance, then scales the distribution to unit mass
// in place and returns the mass it had. reason is empty when it is usable.
// Invalid probabilities are refused, never repaired.
func normalizeDistribution(distribution []float64, tolerance float64) (mass float64, reason string) {
	for _, p := range distribution {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return 0, "probability_value"
		}
		mass += p
	}
	if mass <= 0 || math.Abs(mass-1) > tolerance {
		return 0, "probability_sum"
	}
	for i := range distribution {
		distribution[i] /= mass
	}
	return mass, ""
}

// levelMean is a score distribution's expected zero-based level index.
func levelMean(distribution []float64) float64 {
	mean := 0.0
	for i, p := range distribution {
		mean += float64(i) * p
	}
	return mean
}

// readSystemOne reads Jev's `/systemone` answer into contract order.
func readSystemOne(body io.Reader, call *provider.Call) (provider.Outcome, []contract.DecisionAnswer, error) {
	var response systemOneResponse
	outcome, reason := decodeDecisionResponse(body, call, systemOneResponseShape, &response, func() decisionResponseFacts {
		return decisionResponseFacts{model: response.Model, input: response.Usage.InputTokens, output: response.Usage.OutputTokens, cost: response.Usage.ReportedCost()}
	})
	fail := func(reason string) (provider.Outcome, []contract.DecisionAnswer, error) {
		return outcome, nil, provider.InvalidDecisionResponse(reason)
	}
	if reason != "" {
		return fail(reason)
	}
	if response.Usage.InputTokens == nil || response.Usage.OutputTokens == nil || *response.Usage.InputTokens < 0 || *response.Usage.OutputTokens < 0 {
		return fail("usage_units")
	}
	if len(response.Answers) != len(call.Decisions.Questions) {
		return fail("answer_count")
	}
	if response.Provider != "TypeSafe" || response.ID == "" {
		return fail("provider_identity")
	}
	answers := make([]contract.DecisionAnswer, 0, len(response.Answers))
	for _, question := range call.Decisions.Questions {
		rawAnswer, ok := response.Answers[question.ID]
		if !ok {
			return fail("answer_identity")
		}
		var wire systemOneAnswer
		if json.Unmarshal(rawAnswer, &wire) != nil {
			return fail("answer_json")
		}
		if wire.Type != question.Kind {
			return fail("answer_kind")
		}
		answer := contract.DecisionAnswer{ID: question.ID, Kind: question.Kind}
		switch question.Kind {
		case "choice":
			answer.Reply = &contract.DecisionReply{Label: wire.Choice}
			answer.Confidence = wire.Confidence
			if len(wire.Probabilities) != len(question.Options) {
				return fail("choice_probability_count")
			}
			for _, option := range question.Options {
				p, ok := wire.Probabilities[option]
				if !ok || p == nil {
					return fail("choice_probability_key")
				}
				answer.Probabilities = append(answer.Probabilities, *p)
			}
		case "score":
			if len(wire.Probabilities) != len(question.Levels) {
				return fail("score_probability_count")
			}
			for i := range question.Levels {
				p, ok := wire.Probabilities[strconv.Itoa(i)]
				if !ok || p == nil {
					return fail("score_probability_key")
				}
				answer.Distribution = append(answer.Distribution, *p)
			}
			// TypeSafe computes Score on normalized mass, even when its wire
			// probabilities retain a small rounding residual. Jev prints a
			// Score answer at two decimals (TypeSafe's published examples and
			// every answer it has served Kaana): each printed probability, and
			// the score, may be up to half a hundredth from the value TypeSafe
			// computed. Only an answer printed wholly on that grid gets the
			// tolerance that rounding implies; any other keeps the contract's
			// 1e-6.
			grid := wire.Score != nil && onSystemOneGrid(*wire.Score)
			for _, p := range answer.Distribution {
				grid = grid && onSystemOneGrid(p)
			}
			tolerance := 1e-6
			if grid {
				tolerance = float64(len(answer.Distribution))*systemOneHalfUnit + 1e-9
			}
			mass, reason := normalizeDistribution(answer.Distribution, tolerance)
			if reason != "" {
				return fail(reason)
			}
			answer.Confidence = wire.Confidence
			if !grid {
				// Keep the actual provider score/reply; the contract still
				// rejects disagreement with the normalized weighted mean.
				answer.Mean = wire.Score
				answer.Reply = &contract.DecisionReply{Score: wire.Score}
				break
			}
			// A printed score agrees with the printed distribution to the
			// precision both were printed in: half a unit for the score itself,
			// plus what rounding each probability can have moved the mean,
			// |level - mean| x half a unit, over the printed mass. The answer
			// then carries that mean, as the OpenAI adapter's does, so the
			// contract's 1e-6 holds on what is returned.
			mean := levelMean(answer.Distribution)
			spread := 0.0
			for i := range answer.Distribution {
				spread += math.Abs(float64(i) - mean)
			}
			if math.Abs(*wire.Score-mean) > systemOneHalfUnit+systemOneHalfUnit*spread/mass+1e-6 {
				return fail("score_distribution")
			}
			answer.Mean = &mean
			answer.Reply = &contract.DecisionReply{Score: &mean}
		case "noul":
			if wire.Confidence != nil || wire.Choice != nil || wire.Score != nil || wire.Probabilities != nil {
				return fail("noul_fields")
			}
			answer.Probability = wire.Noul
		}
		answers = append(answers, answer)
	}
	return outcome, answers, nil
}

// systemOneHalfUnit is half of the hundredth Jev prints its answers in.
const systemOneHalfUnit = 0.005

// onSystemOneGrid reports whether a number was printed at two decimals or
// fewer: 0.57 and 1 are, 0.7499995 is not.
func onSystemOneGrid(value float64) bool {
	scaled := value * 100
	return math.Abs(scaled-math.Round(scaled)) <= 1e-9
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
