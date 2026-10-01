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

// decisionPartialReport is the completed usage fixture, cut short after the
// provider measured it: what a decisions failure with usage carries.
func decisionPartialReport(t *testing.T) UsageReport {
	t.Helper()
	for _, f := range validFixtures(t) {
		if f.Schema == "normalizedUsageReportSchema" && f.Case == "completed" {
			report := f.Value.(UsageReport)
			report.Outcome = OutcomePartial
			report.TimeToFirstTokenMs = nil
			if err := report.Validate(); err != nil {
				t.Fatal(err)
			}
			return report
		}
	}
	t.Fatal("no completed usage fixture")
	return UsageReport{}
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
	partial := decisionPartialReport(t)
	failure := DecisionFailure{SchemaVersion: DecisionSchemaVersion, RequestID: partial.RequestID, Error: *NewError(partial.RequestID, CodeProviderTimeout, "the provider did not answer in time").WithoutRetry(), Usage: &partial}
	unmeasured := failure
	unmeasured.Usage = nil
	completedUsage := partial
	completedUsage.Outcome = OutcomeCompleted
	claimsCompleted := failure
	claimsCompleted.Usage = &completedUsage
	valid = append(valid,
		fixture{Schema: "decisionFailureSchema", Case: "measured-usage", Value: failure},
		fixture{Schema: "decisionFailureSchema", Case: "unmeasured", Value: unmeasured},
	)
	invalid := []fixture{
		{Schema: "decisionFailureSchema", Case: "completed-usage", Value: claimsCompleted},
		{Schema: "decisionAnswerSchema", Case: "wrong-sum", Value: DecisionAnswer{ID: "c", Kind: "choice", Reply: &DecisionReply{Label: pointerTo("b")}, Confidence: pointerTo(0.5), Probabilities: []float64{0.3, 0.3}}},
		{Schema: "decisionAnswerSchema", Case: "wrong-mean", Value: DecisionAnswer{ID: "s", Kind: "score", Reply: &DecisionReply{Score: pointerTo(0.75)}, Confidence: pointerTo(0.5), Mean: pointerTo(0.5), Distribution: []float64{0.25, 0.75}}},
		{Schema: "decisionInputSchema", Case: "byte-budget", Value: DecisionInput{State: strings.Repeat("é", 16384), Questions: input.Questions}},
	}
	return valid, invalid
}

// The expected numbers are Oxy's decisionInputBudget, run on the exact local
// foundation build these inputs were copied into (see docs/decisions.md).
func TestDecisionBudgetMatchesOxy(t *testing.T) {
	repeatedQuestions := make([]DecisionQuestion, 9)
	for k := range repeatedQuestions {
		repeatedQuestions[k] = DecisionQuestion{ID: "q" + string(rune('0'+k)), Kind: "noul", Question: "Q" + string(rune('0'+k))}
	}
	for _, tc := range []struct {
		name    string
		input   DecisionInput
		want    DecisionBudget
		gateway bool
	}{
		{"small", decisionTestInput(), DecisionBudget{696, 461, 4792}, true},
		{"escaped", DecisionInput{State: strings.Repeat("<&>  \"\\\n\b\x01é😀", 50), Questions: []DecisionQuestion{
			{ID: "<id>", Kind: "choice", Question: "q&a", Options: []string{"<x>", "y "}},
			{ID: "n", Kind: "noul", Question: "\t"},
		}}, DecisionBudget{2968, 2915, 7064}, true},
		{"repeated", DecisionInput{State: "s", Instructions: pointerTo(strings.Repeat("i", 3000)), Questions: repeatedQuestions}, DecisionBudget{27941, 3365, 32037}, false},
	} {
		if got := tc.input.Budget(); got != tc.want {
			t.Errorf("%s: budget %+v, Oxy measures %+v", tc.name, got, tc.want)
		}
		if err := tc.input.Validate(); err != nil {
			t.Errorf("%s: Oxy accepts it: %v", tc.name, err)
		}
		if tc.input.FitsGateway() != tc.gateway {
			t.Errorf("%s: gateway fit differs from Oxy", tc.name)
		}
	}
	// Escaping is measured: 5400 raw bytes of '<' are 32400 serialized, which
	// passes a raw-length check and fails the serialized context budget.
	escaped := DecisionInput{State: strings.Repeat("<", 5400), Questions: []DecisionQuestion{{ID: "n", Kind: "noul", Question: "?"}}}
	if escaped.Validate() == nil {
		t.Error("escaped state measured by raw length")
	}
	escaped.State = strings.Repeat("a", 5400)
	if err := escaped.Validate(); err != nil {
		t.Errorf("unescaped positive control: %v", err)
	}
}

func TestDecisionFailureValidation(t *testing.T) {
	report := decisionPartialReport(t)
	good := DecisionFailure{SchemaVersion: DecisionSchemaVersion, RequestID: report.RequestID, Error: *NewError(report.RequestID, CodeProviderError, "synthetic"), Usage: &report}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	unmeasured := good
	unmeasured.Usage = nil
	if err := unmeasured.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*DecisionFailure){
		func(f *DecisionFailure) { f.SchemaVersion = 2 },
		func(f *DecisionFailure) { f.Error.RequestID = "req_other" },
		func(f *DecisionFailure) { u := *f.Usage; u.Outcome = OutcomeCompleted; f.Usage = &u },
		func(f *DecisionFailure) { u := *f.Usage; u.RequestID = "req_other"; f.Usage = &u },
	} {
		f := good
		mutate(&f)
		if f.Validate() == nil {
			t.Error("invalid decisions failure accepted")
		}
	}
	e := NewError("req_x", CodeProviderTimeout, "x").WithRetryAfter(5).WithoutRetry()
	if e.Retryable || e.RetryAfterMs != nil {
		t.Fatal("WithoutRetry left a retry signal")
	}
	if NewError("req_x", CodeInvalidRequest, "x").WithoutRetry().Retryable {
		t.Fatal("WithoutRetry widened")
	}
}
