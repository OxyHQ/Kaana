package openaicompat

import (
	"errors"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Public TypeSafe Score/Noul shapes composed into Mention's four-question
// shape. Synthetic values, not a capture of the failed production response.
func TestDecisionsMentionShapeAndFiniteDiagnostics(t *testing.T) {
	req, route := decisionFixture("openrouter")
	req.Input.Decisions.Questions = []contract.DecisionQuestion{
		{ID: "spam", Kind: "noul", Question: "Spam?"},
		{ID: "repetition", Kind: "noul", Question: "Repetitive?"},
		{ID: "language:0", Kind: "noul", Question: "English?"},
		{ID: "feedScore", Kind: "score", Question: "Rate", Levels: []string{"zero", "one", "two", "three", "four"}},
	}
	call := &provider.Call{Route: route, Decisions: req.Input.Decisions}
	a := &Adapter{config: Config{Provider: "openrouter"}}
	good := `{"model":"typesafe/jev-1.13-20260917","id":"synthetic-generation","provider":"TypeSafe","answers":{"spam":{"type":"noul","noul":0},"repetition":{"type":"noul","noul":0.1},"language:0":{"type":"noul","noul":1},"feedScore":{"type":"score","score":1.43,"confidence":0.35,"legend":{"0":"zero","1":"one","2":"two","3":"three","4":"four"},"probabilities":{"0":0,"1":0.57,"2":0.43,"3":0,"4":0}}},"usage":{"input_tokens":618,"output_tokens":72,"cost":0.000025956}}`
	out, err := a.readDecisions(strings.NewReader(good), call)
	if err != nil || len(out.Decisions) != 4 || out.FinishReason != contract.FinishStop {
		t.Fatalf("documented four-question positive: %v", err)
	}
	for _, tt := range []struct{ name, old, replacement, reason string }{
		{"model", `"model":"typesafe/jev-1.13-20260917"`, `"model":"PRIVATE_MODEL_CANARY"`, "model_identity"},
		{"provider", `"provider":"TypeSafe"`, `"provider":"PRIVATE_PROVIDER_CANARY"`, "provider_identity"},
		{"id", `"id":"synthetic-generation"`, `"id":""`, "provider_identity"},
		{"count", `"spam":{"type":"noul","noul":0},`, ``, "answer_count"},
		{"question", `"spam":{`, `"PRIVATE_QUESTION_CANARY":{`, "answer_identity"},
		{"type", `"spam":{"type":"noul"`, `"spam":{"type":"score"`, "answer_kind"},
		{"decode", `"spam":{"type":"noul","noul":0}`, `"spam":{"type":"noul","noul":"PRIVATE_ANSWER_CANARY"}`, "answer_json"},
		{"noul_extra", `"noul":0}`, `"noul":0,"confidence":0.5}`, "noul_fields"},
		{"noul_range", `"noul":0}`, `"noul":1.1}`, "noul_value"},
		{"noul_missing", `"noul":0}`, `"noul":null}`, "noul_value"},
		{"score_count", `"4":0}`, `"4":0,"5":0}`, "score_probability_count"},
		{"score_key", `"4":0}`, `"PRIVATE_LEVEL_CANARY":0}`, "score_probability_key"},
		{"score_null", `"4":0}`, `"4":null}`, "score_probability_key"},
		{"confidence", `"confidence":0.35`, `"confidence":null`, "reply_confidence"},
		{"range", `"1":0.57`, `"1":1.1`, "probability_value"},
		{"sum", `"1":0.57`, `"1":0.56`, "probability_sum"},
		{"mean", `"score":1.43`, `"score":1.44`, "score_distribution"},
		{"score_missing", `"score":1.43`, `"score":null`, "score_value"},
		{"duplicate", `"noul":0}`, `"noul":0,"noul":0}`, "ambiguous_json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(good, tt.old) {
				t.Fatal("mutation did not apply")
			}
			mutated := strings.Replace(good, tt.old, tt.replacement, 1)
			got, err := a.readDecisions(strings.NewReader(mutated), call)
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) || failure.Code != contract.CodeProviderError || failure.Category != contract.UpstreamUnknown || failure.Detail != "invalid systemone response: "+tt.reason {
				t.Fatalf("wrong finite diagnostic: %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE_") || len(got.Decisions) != 0 {
				t.Fatal("payload escaped or invalid answers accepted")
			}
			if got.ProviderReportedCost == nil || got.ProviderReportedCost.Amount != 25956000 || len(got.Units) != 3 || got.Units[0].Quantity != 618 || got.Units[1].Quantity != 72 || got.Units[2].Quantity != 1 {
				t.Fatalf("measured work lost: %+v", got)
			}
		})
	}
}

func TestDecisionDiagnosticUnknownValidatorErrorIsClosed(t *testing.T) {
	reason := decisionAnswerFailureReason(errors.New("PRIVATE_UNRECOGNIZED_ERROR_CANARY"))
	if reason != "answer_contract" || provider.DecisionResponseDiagnostic("invalid systemone response: "+reason) != reason {
		t.Fatal("unknown validator error did not use fixed fallback")
	}
}
