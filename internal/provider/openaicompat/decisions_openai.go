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

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// OpenAI's Decisions API, `POST /v1/decisions` (public beta since 2026-10-07,
// https://developers.openai.com/api/docs/guides/decisions), on the
// `openai-decisions` slug. One request asks every question about one input;
// OpenAI bills its input tokens only. See docs/decisions.md for the mapping
// and for what activating it still needs.

// openAIDecisionsRequest is the request body. Each contract question becomes
// one named question; `noul` is OpenAI's `predicate`.
type openAIDecisionsRequest struct {
	Model     string                   `json:"model"`
	Input     string                   `json:"input"`
	Questions []openAIDecisionQuestion `json:"questions"`
}

type openAIDecisionQuestion struct {
	Type         string                 `json:"type"`
	Name         string                 `json:"name"`
	Instructions string                 `json:"instructions"`
	Choices      []openAIDecisionChoice `json:"choices,omitempty"`
	Levels       []openAIDecisionLevel  `json:"levels,omitempty"`
}

// The contract names an option or a level by its text alone, so that text is
// both its identity and its description: nothing is invented beside it.
type openAIDecisionChoice struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

type openAIDecisionLevel struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

var openAIQuestionType = map[contract.DecisionKind]string{"noul": "predicate", "choice": "choice", "score": "score"}

func openAIDecisionQuestions(input *contract.DecisionInput, questions []contract.DecisionQuestion) []openAIDecisionQuestion {
	wire := make([]openAIDecisionQuestion, 0, len(questions))
	for _, q := range questions {
		// The shared instructions, the question and its rubric, in that order:
		// OpenAI's question carries one instructions text.
		parts := make([]string, 0, 3)
		if input.Instructions != nil {
			parts = append(parts, *input.Instructions)
		}
		parts = append(parts, q.Question)
		if q.Criteria != nil {
			parts = append(parts, *q.Criteria)
		}
		question := openAIDecisionQuestion{Type: openAIQuestionType[q.Kind], Name: q.ID, Instructions: strings.Join(parts, "\n\n")}
		for _, option := range q.Options {
			question.Choices = append(question.Choices, openAIDecisionChoice{Value: option, Description: option})
		}
		for _, level := range q.Levels {
			question.Levels = append(question.Levels, openAIDecisionLevel{Label: level, Description: level})
		}
		wire = append(wire, question)
	}
	return wire
}

func (a *Adapter) translateOpenAIDecisions(request *contract.Request, route provider.Route) (*provider.Call, error) {
	input := request.Input.Decisions
	encode := func(questions []contract.DecisionQuestion) ([]byte, error) {
		return json.Marshal(openAIDecisionsRequest{Model: route.UpstreamModelID, Input: input.State, Questions: openAIDecisionQuestions(input, questions)})
	}
	encoded, err := encode(input.Questions)
	if err != nil {
		return nil, fmt.Errorf("decisions: encode request: %w", err)
	}
	// The contract's budgets bound the body as sent, whole and per question.
	tooLarge := provider.ErrUnsupported{Code: contract.CodeRequestTooLarge, Param: "input.decisions", Detail: "the serialized decisions body exceeds its input budget"}
	if len(encoded) > contract.DecisionTotalBudget {
		return nil, tooLarge
	}
	for i := range input.Questions {
		single, err := encode(input.Questions[i : i+1])
		if err != nil {
			return nil, fmt.Errorf("decisions: encode request: %w", err)
		}
		if len(single) > contract.DecisionContextBudget {
			return nil, tooLarge
		}
	}
	return &provider.Call{RequestID: request.Attribution.RequestID, Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/decisions", Body: encoded, Header: http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}, Decisions: input}, nil
}

type openAIDecisionsResponse struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Answers []json.RawMessage    `json:"answers"`
	Usage   openAIDecisionsUsage `json:"usage"`
}

type openAIDecisionsUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

type openAIDecisionAnswer struct {
	Type          string                      `json:"type"`
	Name          string                      `json:"name"`
	Probability   *float64                    `json:"probability"`
	Choice        *string                     `json:"choice"`
	Score         *json.Number                `json:"score"`
	Confidence    *float64                    `json:"confidence"`
	Probabilities []openAIDecisionProbability `json:"probabilities"`
}

// openAIDecisionProbability is one entry of a choice's or a score's
// distribution: a choice names its value; a score names its level's label
// and, as value, its zero-based index.
type openAIDecisionProbability struct {
	Value       json.RawMessage `json:"value"`
	Label       *string         `json:"label"`
	Probability *float64        `json:"probability"`
}

var openAIDecisionsResponseShape = &jsonShape{fields: map[string]*jsonShape{
	"id": nil, "model": nil,
	"usage": {usage: true, fields: map[string]*jsonShape{"input_tokens": nil, "output_tokens": nil}},
	"answers": {values: &jsonShape{fields: map[string]*jsonShape{
		"type": nil, "name": nil, "probability": nil, "choice": nil, "score": nil, "confidence": nil,
		"probabilities": {values: &jsonShape{fields: map[string]*jsonShape{"value": nil, "label": nil, "probability": nil}}},
	}}},
}}

// readOpenAIDecisions reads one Decisions API answer under the same rules as
// readDecisions: measured units survive an invalid answer once the body
// decodes, a refusal is a content filter, and every answer is validated
// against the request before it is returned.
func (a *Adapter) readOpenAIDecisions(body io.Reader, call *provider.Call) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported}
	fail := func(reason string) (provider.Outcome, error) {
		return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "invalid decisions response: " + reason}
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, (2<<20)+1))
	if readErr != nil || len(raw) > 2<<20 {
		return fail("body_read")
	}
	var response openAIDecisionsResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&response); err != nil {
		return fail("response_json")
	}
	ambiguity := unambiguousDecisionJSON(json.NewDecoder(bytes.NewReader(raw)), openAIDecisionsResponseShape)
	if !errors.Is(ambiguity, errAmbiguousUsage) {
		if response.Usage.InputTokens != nil && *response.Usage.InputTokens >= 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: *response.Usage.InputTokens})
		}
		if response.Usage.OutputTokens != nil && *response.Usage.OutputTokens >= 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: *response.Usage.OutputTokens})
		}
	}
	outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitRequests, Quantity: 1})
	if ambiguity != nil {
		return fail("ambiguous_json")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fail("trailing_json")
	}
	if call.Decisions == nil {
		return fail("missing_questions")
	}
	if response.Model != call.Route.UpstreamModelID {
		return fail("model_identity")
	}
	if response.Usage.InputTokens == nil || *response.Usage.InputTokens < 0 || (response.Usage.OutputTokens != nil && *response.Usage.OutputTokens < 0) {
		return fail("usage_units")
	}
	wire := make(map[string]openAIDecisionAnswer, len(response.Answers))
	for _, rawAnswer := range response.Answers {
		var answer openAIDecisionAnswer
		if json.Unmarshal(rawAnswer, &answer) != nil {
			return fail("answer_json")
		}
		if _, repeated := wire[answer.Name]; repeated {
			return fail("answer_identity")
		}
		wire[answer.Name] = answer
	}
	for _, answer := range wire {
		if answer.Type == "refusal" {
			// The model declined to answer: the provider's content filter, with
			// the work it measured.
			return outcome, provider.ErrUpstream{Code: contract.CodeUpstreamContentFiltered, Category: contract.UpstreamContentFilter, Detail: "the decisions model refused a question"}
		}
	}
	if len(wire) != len(call.Decisions.Questions) {
		return fail("answer_count")
	}
	answers := make([]contract.DecisionAnswer, 0, len(wire))
	for _, question := range call.Decisions.Questions {
		got, ok := wire[question.ID]
		if !ok {
			return fail("answer_identity")
		}
		if got.Type != openAIQuestionType[question.Kind] {
			return fail("answer_kind")
		}
		answer := contract.DecisionAnswer{ID: question.ID, Kind: question.Kind}
		switch question.Kind {
		case "noul":
			answer.Probability = got.Probability
		case "choice":
			distribution, reason := openAIDistribution(got.Probabilities, question.Options, func(p openAIDecisionProbability) (int, bool) {
				var value string
				if json.Unmarshal(p.Value, &value) != nil {
					return 0, false
				}
				return indexOf(question.Options, value)
			})
			if reason != "" {
				return fail("choice_" + reason)
			}
			answer.Reply = &contract.DecisionReply{Label: got.Choice}
			answer.Confidence = got.Confidence
			answer.Probabilities = distribution
		case "score":
			distribution, reason := openAIDistribution(got.Probabilities, question.Levels, func(p openAIDecisionProbability) (int, bool) {
				var index int
				hasIndex := len(p.Value) > 0 && json.Unmarshal(p.Value, &index) == nil
				if p.Label == nil {
					return index, hasIndex
				}
				position, known := indexOf(question.Levels, *p.Label)
				return position, known && (!hasIndex || index == position)
			})
			if reason != "" {
				return fail("score_" + reason)
			}
			if got.Score == nil {
				return fail("reply_confidence")
			}
			score, err := got.Score.Float64()
			if err != nil {
				return fail("score_value")
			}
			mean := 0.0
			for i, p := range distribution {
				mean += float64(i) * p
			}
			// OpenAI's score is the probability-weighted mean of the zero-based
			// level indices, which it may print rounded. It must agree with the
			// mean of the distribution to the precision it was printed in; the
			// answer is then that mean, exactly as readDecisions answers the
			// mean of Jev's normalized mass, so the contract's 1e-6 rules hold
			// on what is returned. The distribution itself gets no tolerance
			// beyond the contract's.
			if math.Abs(score-mean) > printedHalfUnit(*got.Score)+1e-6 {
				return fail("score_reply")
			}
			answer.Distribution = distribution
			answer.Mean = &mean
			answer.Reply = &contract.DecisionReply{Score: &mean}
			answer.Confidence = got.Confidence
		}
		answers = append(answers, answer)
	}
	if err := contract.ValidateDecisionAnswers(*call.Decisions, answers); err != nil {
		return fail(decisionAnswerFailureReason(err))
	}
	outcome.Decisions = answers
	outcome.FinishReason = contract.FinishStop
	return outcome, nil
}

// openAIDistribution puts a choice's or a score's probabilities into the
// contract's order (`position` names each entry's place), checks every one is
// a probability and that they sum to one within the contract's 1e-6, and
// returns them normalized, as readDecisions does Jev's. reason is empty when
// the distribution is usable.
func openAIDistribution(entries []openAIDecisionProbability, labels []string, position func(openAIDecisionProbability) (int, bool)) (distribution []float64, reason string) {
	if len(entries) != len(labels) {
		return nil, "probability_count"
	}
	distribution = make([]float64, len(labels))
	seen := make([]bool, len(labels))
	mass := 0.0
	for _, entry := range entries {
		index, ok := position(entry)
		if !ok || index < 0 || index >= len(labels) || seen[index] || entry.Probability == nil {
			return nil, "probability_key"
		}
		seen[index] = true
		p := *entry.Probability
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return nil, "probability_value"
		}
		distribution[index] = p
		mass += p
	}
	if math.Abs(mass-1) > 1e-6 {
		return nil, "probability_sum"
	}
	for i := range distribution {
		distribution[i] /= mass
	}
	return distribution, ""
}

// printedHalfUnit is half the last printed fractional digit of a JSON number:
// the most rounding to that precision can have moved it. `2.37` → 0.005,
// `1.5e-7` → 5e-9; a number printed without a fraction is taken as exact.
func printedHalfUnit(number json.Number) float64 {
	text := strings.ToLower(string(number))
	exponent := 0
	if mantissa, power, found := strings.Cut(text, "e"); found {
		parsed, err := strconv.Atoi(power)
		if err != nil {
			return 0
		}
		text, exponent = mantissa, parsed
	}
	_, fraction, found := strings.Cut(text, ".")
	if !found {
		return 0
	}
	return 0.5 * math.Pow10(exponent-len(fraction))
}

func indexOf(labels []string, label string) (int, bool) {
	for i, candidate := range labels {
		if candidate == label {
			return i, true
		}
	}
	return 0, false
}
