package contract

import (
	"math"
	"strings"
	"testing"
)

func decisionTestInput() DecisionInput {
	return DecisionInput{State: "synthetic", Instructions: pointerTo("Use rubric"), Effort: pointerTo(DecisionEffort("instant")), Questions: []DecisionQuestion{
		{ID: "c", Kind: "choice", Question: "Select", Criteria: pointerTo("Exclusive"), Options: []string{"a", "b"}},
		{ID: "s", Kind: "score", Question: "Rate", Criteria: pointerTo("Ordered"), Levels: []string{"low", "high"}},
		{ID: "n", Kind: "noul", Question: "True?", Criteria: pointerTo("Binary")},
	}}
}
func decisionTestAnswers() []DecisionAnswer {
	return []DecisionAnswer{{ID: "c", Kind: "choice", Reply: &DecisionReply{Label: pointerTo("b")}, Confidence: pointerTo(0.5), Probabilities: []float64{0.25, 0.75}}, {ID: "s", Kind: "score", Reply: &DecisionReply{Score: pointerTo(0.75)}, Confidence: pointerTo(0.5), Mean: pointerTo(0.75), Distribution: []float64{0.25, 0.75}}, {ID: "n", Kind: "noul", Probability: pointerTo(0.0)}}
}
func TestDecisionsValidation(t *testing.T) {
	good := decisionTestInput()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*DecisionInput){
		func(d *DecisionInput) { d.Questions[1].ID = "c" },
		func(d *DecisionInput) { d.Questions[0].Options = []string{"a", "a"} },
		func(d *DecisionInput) { d.Questions[1].Levels = []string{"a"} },
		func(d *DecisionInput) { d.Questions[2].Options = []string{"a", "b"} },
		func(d *DecisionInput) { d.State = strings.Repeat("é", 16384) },
		func(d *DecisionInput) { d.Questions = nil },
		func(d *DecisionInput) { d.Effort = pointerTo(DecisionEffort("max")) },
	} {
		d := decisionTestInput()
		mutate(&d)
		if err := d.Validate(); err == nil {
			t.Error("invalid input accepted")
		}
	}
	answers := decisionTestAnswers()
	if err := ValidateDecisionAnswers(good, answers); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]DecisionAnswer){
		func(a []DecisionAnswer) { a[1].ID = "c" },
		func(a []DecisionAnswer) { a[1].Kind = "choice" },
		func(a []DecisionAnswer) { a[0].Probabilities = []float64{1} },
		func(a []DecisionAnswer) { a[0].Probabilities[0] = math.NaN() },
		func(a []DecisionAnswer) { a[0].Probabilities[0] = 0.5 },
		func(a []DecisionAnswer) { a[1].Mean = pointerTo(0.6) },
		func(a []DecisionAnswer) { a[2].Probability = pointerTo(math.Inf(1)) },
		func(a []DecisionAnswer) { a[2].Probability = nil },
	} {
		a := decisionTestAnswers()
		mutate(a)
		if err := ValidateDecisionAnswers(good, a); err == nil {
			t.Error("invalid answer accepted")
		}
	}
}

func decisionWireFixtures(t *testing.T) ([]fixture, []fixture) {
	input := decisionTestInput()
	answers := decisionTestAnswers()
	valid := []fixture{{Schema: "decisionInputSchema", Case: "typed-input", Value: input}}
	for i, q := range input.Questions {
		valid = append(valid, fixture{Schema: "decisionQuestionSchema", Case: string(q.Kind), Value: q}, fixture{Schema: "decisionAnswerSchema", Case: string(q.Kind), Value: answers[i]})
	}
	for _, f := range validFixtures(t) {
		if f.Schema == "inferenceRequestSchema" && f.Case == "messages-with-every-optional-field" {
			r := f.Value.(Request)
			r.Client.APIFormat = APIFormatDecisions
			r.Input = Input{Format: InputDecisions, Decisions: &input}
			r.Stream = false
			r.Target = RoutingTarget{Kind: TargetModel, ModelReference: pointerTo(r.AuthorizedRoutes[0].ModelReference)}
			r.AuthorizedRoutes = r.AuthorizedRoutes[:1]
			r.MaxOutputTokens, r.Reasoning, r.ToolChoice, r.ResponseFormat = nil, nil, nil, nil
			r.Sampling = SamplingParameters{}
			r.Tools = nil
			valid = append(valid, fixture{Schema: "inferenceRequestSchema", Case: "decisions-envelope", Value: r})
		}
		if f.Schema == "normalizedUsageReportSchema" && f.Case == "completed" {
			usage := f.Value.(UsageReport)
			valid = append(valid, fixture{Schema: "decisionResultSchema", Case: "typed-result", Value: DecisionResult{SchemaVersion: 1, RequestID: usage.RequestID, Model: usage.ResolvedModelReference, Data: answers, Usage: usage}})
		}
	}
	invalid := []fixture{
		{Schema: "decisionAnswerSchema", Case: "wrong-sum", Value: DecisionAnswer{ID: "c", Kind: "choice", Reply: &DecisionReply{Label: pointerTo("b")}, Confidence: pointerTo(0.5), Probabilities: []float64{0.3, 0.3}}},
		{Schema: "decisionAnswerSchema", Case: "wrong-mean", Value: DecisionAnswer{ID: "s", Kind: "score", Reply: &DecisionReply{Score: pointerTo(0.75)}, Confidence: pointerTo(0.5), Mean: pointerTo(0.5), Distribution: []float64{0.25, 0.75}}},
		{Schema: "decisionInputSchema", Case: "byte-budget", Value: DecisionInput{State: strings.Repeat("é", 16384), Questions: input.Questions}},
	}
	return valid, invalid
}
