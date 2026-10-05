package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Synthetic wire values follow the official TypeSafe adapter's distinction
// between raw probabilities and a score calculated on their normalized mass.
// They are not the lost production response.
func TestScoreNormalizedMassRealWire(t *testing.T) {
	for _, tc := range []struct {
		name          string
		probabilities string
		score         float64
		reason        string
	}{
		{"mass_below_one", `{"0":0,"1":0.25,"2":0,"3":0,"4":0.7499995}`, 3.249998 / 0.9999995, ""},
		{"mass_above_one", `{"0":0,"1":0.2500005,"2":0,"3":0,"4":0.75}`, 3.2500005 / 1.0000005, ""},
		{"exact_mass", `{"0":0,"1":0.25,"2":0,"3":0,"4":0.75}`, 3.25, ""},
		{"wrong_score", `{"0":0,"1":0.25,"2":0,"3":0,"4":0.7499995}`, 3.24, "score_distribution"},
		{"raw_unnormalized_score", `{"0":0,"1":0.25,"2":0,"3":0,"4":0.7499995}`, 3.249998, "score_distribution"},
		{"outside_mass_tolerance", `{"0":0,"1":0.25,"2":0,"3":0,"4":0.749998}`, 3.249992 / 0.999998, "probability_sum"},
		{"zero_mass", `{"0":0,"1":0,"2":0,"3":0,"4":0}`, 0, "probability_sum"},
		{"negative_probability", `{"0":-0.0000001,"1":0.25,"2":0,"3":0,"4":0.7500001}`, 3.25, "probability_value"},
		{"above_one_probability", `{"0":0,"1":0,"2":0,"3":0,"4":1.0000001}`, 4, "probability_value"},
		{"missing_level", `{"0":0,"1":0.25,"2":0,"3":0}`, 3.25, "score_probability_count"},
		{"foreign_level", `{"0":0,"1":0.25,"2":0,"3":0,"5":0.75}`, 3.25, "score_probability_key"},
		{"null_level", `{"0":0,"1":0.25,"2":0,"3":0,"4":null}`, 3.25, "score_probability_key"},
		{"nonfinite_number", `{"0":0,"1":0.25,"2":0,"3":0,"4":1e999}`, 3.25, "ambiguous_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, route := decisionFixture("openrouter")
			req.Input.Decisions.Questions = []contract.DecisionQuestion{
				{ID: "score", Kind: "score", Question: "Rate synthetic content", Levels: []string{"zero", "one", "two", "three", "four"}},
				{ID: "noul", Kind: "noul", Question: "Synthetic yes/no?"},
			}
			var calls atomic.Int32
			a := decisionAdapter(t, "openrouter", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/systemone" {
					t.Error("unexpected wire method/path")
				}
				_, _ = fmt.Fprintf(w, `{"model":"%s","id":"synthetic-normalized-score","provider":"TypeSafe","answers":{"score":{"type":"score","score":%.17g,"confidence":0.5,"probabilities":%s},"noul":{"type":"noul","noul":0.125}},"usage":{"input_tokens":12,"output_tokens":4,"cost":0.000000000123}}`, route.UpstreamModelID, tc.score, tc.probabilities)
			})
			a.decisions = decisionReview{true, true, true, true, true}
			call, err := a.Translate(req, route)
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.Stream(context.Background(), call, nil, nil)
			if calls.Load() != 1 {
				t.Fatalf("provider calls=%d", calls.Load())
			}
			if out.ProviderReportedCost == nil || out.ProviderReportedCost.Amount != 123 || len(out.Units) != 3 || out.Units[0].Quantity != 12 || out.Units[1].Quantity != 4 || out.Units[2].Quantity != 1 {
				t.Fatal("measured usage/cost lost")
			}
			if tc.reason != "" {
				var failure provider.ErrUpstream
				if !errors.As(err, &failure) || provider.DecisionResponseDiagnostic(failure.Detail) != tc.reason || len(out.Decisions) != 0 {
					t.Fatalf("expected %s rejection, got %v", tc.reason, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Decisions) != 2 || out.Decisions[0].Mean == nil || *out.Decisions[0].Mean != tc.score || out.Decisions[0].Reply == nil || *out.Decisions[0].Reply.Score != tc.score || *out.Decisions[1].Probability != 0.125 {
				t.Fatal("provider score/reply or Noul changed")
			}
			sum := 0.0
			for _, p := range out.Decisions[0].Distribution {
				sum += p
			}
			if math.Abs(sum-1) > 1e-15 {
				t.Fatalf("mass not normalized: %.17g", sum)
			}
			// Validate the serialized output with the unchanged downstream Go contract.
			encoded, err := json.Marshal(out.Decisions)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip []contract.DecisionAnswer
			if err = json.Unmarshal(encoded, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if err = contract.ValidateDecisionAnswers(*req.Input.Decisions, roundtrip); err != nil {
				t.Fatal(err)
			}
			if dir := os.Getenv("KAANA_SCORE_FIXTURE_DIR"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.name+".json"), encoded, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
