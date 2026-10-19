package openaicompat

import (
	"encoding/json"
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
	encoded, err := encodeDecisionsBody(input.Questions, func(questions []contract.DecisionQuestion) ([]byte, error) {
		return json.Marshal(openAIDecisionsRequest{Model: route.UpstreamModelID, Input: input.State, Questions: openAIDecisionQuestions(input, questions)})
	})
	if err != nil {
		return nil, err
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

// readOpenAIDecisions reads one Decisions API answer into contract order. A
// refusal is the provider's content filter, with the work it measured.
func readOpenAIDecisions(body io.Reader, call *provider.Call) (provider.Outcome, []contract.DecisionAnswer, error) {
	var response openAIDecisionsResponse
	outcome, reason := decodeDecisionResponse(body, call, openAIDecisionsResponseShape, &response, func() decisionResponseFacts {
		return decisionResponseFacts{model: response.Model, input: response.Usage.InputTokens, output: response.Usage.OutputTokens}
	})
	fail := func(reason string) (provider.Outcome, []contract.DecisionAnswer, error) {
		return outcome, nil, provider.InvalidDecisionResponse(reason)
	}
	if reason != "" {
		return fail(reason)
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
			return outcome, nil, provider.ErrUpstream{Code: contract.CodeUpstreamContentFiltered, Category: contract.UpstreamContentFilter, Detail: "the decisions model refused a question"}
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
			distribution, reason := openAIDistribution(got.Probabilities, question.Options, "choice", func(p openAIDecisionProbability) (int, bool) {
				var value string
				if json.Unmarshal(p.Value, &value) != nil {
					return 0, false
				}
				return indexOf(question.Options, value)
			})
			if reason != "" {
				return fail(reason)
			}
			answer.Reply = &contract.DecisionReply{Label: got.Choice}
			answer.Confidence = got.Confidence
			answer.Probabilities = distribution
		case "score":
			distribution, reason := openAIDistribution(got.Probabilities, question.Levels, "score", func(p openAIDecisionProbability) (int, bool) {
				var index int
				hasIndex := len(p.Value) > 0 && json.Unmarshal(p.Value, &index) == nil
				if p.Label == nil {
					return index, hasIndex
				}
				position, known := indexOf(question.Levels, *p.Label)
				return position, known && (!hasIndex || index == position)
			})
			if reason != "" {
				return fail(reason)
			}
			if got.Score == nil {
				return fail("reply_confidence")
			}
			score, err := got.Score.Float64()
			if err != nil {
				return fail("score_value")
			}
			// OpenAI's score is the probability-weighted mean of the zero-based
			// level indices, which it may print rounded. It must agree with the
			// mean of the distribution to the precision it was printed in; the
			// answer is then that mean, as Jev's on-grid answers carry theirs, so
			// the contract's 1e-6 rules hold on what is returned. The
			// distribution itself gets no tolerance beyond the contract's.
			mean := levelMean(distribution)
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
	return outcome, answers, nil
}

// openAIDistribution puts a choice's or a score's probabilities into the
// contract's order (`position` names each entry's place) and normalizes them
// within the contract's 1e-6 (normalizeDistribution). kind prefixes the
// reasons that are about the entries themselves; reason is empty when the
// distribution is usable.
func openAIDistribution(entries []openAIDecisionProbability, labels []string, kind string, position func(openAIDecisionProbability) (int, bool)) (distribution []float64, reason string) {
	if len(entries) != len(labels) {
		return nil, kind + "_probability_count"
	}
	distribution = make([]float64, len(labels))
	seen := make([]bool, len(labels))
	for _, entry := range entries {
		index, ok := position(entry)
		if !ok || index < 0 || index >= len(labels) || seen[index] || entry.Probability == nil {
			return nil, kind + "_probability_key"
		}
		seen[index] = true
		distribution[index] = *entry.Probability
	}
	if _, reason := normalizeDistribution(distribution, 1e-6); reason != "" {
		return nil, reason
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
