package openaicompat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// decisionFixture is a decisions request pinned to Jev on OpenRouter, with the
// route's price ceiling the executor would set from its rate card.
func decisionFixture() (*contract.Request, provider.Route) {
	ref := contract.ModelReference("typesafe/jev-1.13@observed-2026-10-19")
	input := &contract.DecisionInput{State: "synthetic state", Questions: []contract.DecisionQuestion{
		{ID: "choice", Kind: "choice", Question: "Select", Options: []string{"a", "b"}},
		{ID: "score", Kind: "score", Question: "Rate", Levels: []string{"low", "high"}},
		{ID: "noul", Kind: "noul", Question: "True?"},
	}}
	req := requestWith(nil)
	req.Target.ModelReference = &ref
	req.Stream = false
	req.MaxOutputTokens = nil
	req.Sampling = contract.SamplingParameters{}
	req.Input = contract.Input{Format: contract.InputDecisions, Decisions: input}
	req.Client.APIFormat = contract.APIFormatDecisions
	prompt, _ := providercost.ParseDecisionTokenPrice("0.042")
	route := provider.Route{Provider: "openrouter", DeploymentID: "dep_decisions", ModelReference: ref, UpstreamModelID: "typesafe/jev-1.13-20260917", DecisionPriceLimit: &providercost.DecisionPriceLimit{Prompt: prompt}}
	req.AuthorizedRoutes = []contract.AuthorizedRoute{{Provider: route.Provider, DeploymentID: route.DeploymentID, ModelReference: ref, Regions: []contract.Region{}, Substitution: contract.SubstitutionSameModel}}
	return req, route
}

func decisionResponse(model string) string {
	return `{"model":"` + model + `","id":"synthetic-generation","provider":"TypeSafe","answers":{"noul":{"type":"noul","noul":0},"score":{"type":"score","score":0.75,"confidence":0.5,"probabilities":{"1":0.75,"0":0.25}},"choice":{"type":"choice","choice":"b","confidence":0.5,"probabilities":{"b":0.75,"a":0.25}}},"usage":{"input_tokens":12,"output_tokens":4,"cost":0.000000000123}}`
}

func decisionAdapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)
	a, err := New(Config{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: identityBoundFakeClient(t, fake.URL)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDecisionsMalformedAnswersPreserveMeasuredWork(t *testing.T) {
	req, route := decisionFixture()
	a := &Adapter{config: Config{Provider: "openrouter"}}
	call := &provider.Call{Route: route, Decisions: req.Input.Decisions}
	good := decisionResponse(route.UpstreamModelID)
	for _, pair := range [][2]string{
		{`"model":"` + route.UpstreamModelID + `"`, `"model":"typesafe/jev-latest"`},
		{`"provider":"TypeSafe"`, `"provider":"Other"`},
		{`"id":"synthetic-generation"`, `"id":""`},
		{`"noul":0`, `"noul":1.01`},
		{`"noul":0`, `"noul":null`},
		{`"noul":{"type"`, `"unexpected":{"type"`},
		{`"score":0.75`, `"score":0.5`},
		{`"b":0.75`, `"b":0.5`},
		{`"a":0.25`, `"a":null`},
		{`"a":0.25`, `"a":"invalid"`},
		{`"confidence":0.5`, `"confidence":null`},
		{`"choice":"b"`, `"choice":"a"`},
		{`"a":0.25`, `"a":0.25,"a":0.25`},
		{`"b":0.75`, `"c":0.75`},
		// Go's struct decoding folds member names, so these are duplicates too.
		{`"model":`, `"MODEL":"typesafe/jev-latest","model":`},
		{`"confidence":0.5,"probabilities":{"b"`, `"Confidence":0.9,"confidence":0.5,"probabilities":{"b"`},
		{`"score":0.75`, `"\u017fcore":0.25,"score":0.75`},
		{`"model":`, `"Model":`},
	} {
		out, err := a.readDecisions(strings.NewReader(strings.Replace(good, pair[0], pair[1], 1)), call)
		if err == nil || out.Decisions != nil || len(out.Units) != 3 {
			t.Errorf("mutation %s: err=%v units=%v", pair[0], err, out.Units)
		}
	}
	if _, err := a.readDecisions(strings.NewReader(good), call); err != nil {
		t.Fatalf("positive control: %v", err)
	}
}
