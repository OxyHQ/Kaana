package openaicompat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

func TestDecisionsUnknownCostKeepsPaidAnswer(t *testing.T) {
	req, route := decisionFixture("openrouter")
	a := &Adapter{config: Config{Provider: "openrouter"}}
	call := &provider.Call{Route: route, Decisions: req.Input.Decisions}
	good := decisionResponse(route.UpstreamModelID)
	for _, tc := range []struct {
		cost  string
		known bool
		want  int64
	}{
		{`"cost":null`, false, 0}, {`"cost":"0.5"`, false, 0}, {`"cost":-1`, false, 0},
		{`"cost":1e-13`, false, 0}, {`"cost":0.0000000000001`, false, 0}, {`"cost":{}`, false, 0},
		{`"cost":1.5e-7`, true, 150000}, {`"cost":0`, true, 0}, {`"cost":0.000000000123`, true, 123},
	} {
		body := strings.Replace(good, `"cost":0.000000000123`, tc.cost, 1)
		out, err := a.readDecisions(strings.NewReader(body), call)
		if err != nil || len(out.Decisions) != 3 || len(out.Units) != 3 {
			t.Errorf("%s discarded a paid answer: %v", tc.cost, err)
			continue
		}
		if !tc.known && out.ProviderReportedCost != nil {
			t.Errorf("%s fabricated amount %v", tc.cost, out.ProviderReportedCost)
		}
		if tc.known && (out.ProviderReportedCost == nil || out.ProviderReportedCost.Amount != tc.want) {
			t.Errorf("%s: amount %v, want %d", tc.cost, out.ProviderReportedCost, tc.want)
		}
	}
	absent := strings.Replace(good, `,"cost":0.000000000123`, "", 1)
	if out, err := a.readDecisions(strings.NewReader(absent), call); err != nil || out.ProviderReportedCost != nil {
		t.Errorf("absent cost: %v %v", out.ProviderReportedCost, err)
	}
}

func TestDecisionsAmbiguousUsageIsUnmeasured(t *testing.T) {
	req, route := decisionFixture("typesafe")
	a := &Adapter{config: Config{Provider: "typesafe"}}
	call := &provider.Call{Route: route, Decisions: req.Input.Decisions}
	good := decisionResponse(route.UpstreamModelID)
	for _, mutated := range []string{
		strings.Replace(good, `"input_tokens":12`, `"INPUT_TOKENS":99999,"input_tokens":12`, 1),
		strings.Replace(good, `"usage":{`, `"Usage":{"input_tokens":1},"usage":{`, 1),
	} {
		out, err := a.readDecisions(strings.NewReader(mutated), call)
		if err == nil || out.Decisions != nil || out.ProviderReportedCost != nil {
			t.Fatalf("ambiguous usage accepted: %v", err)
		}
		if len(out.Units) != 1 || out.Units[0].Unit != contract.UnitRequests {
			t.Fatalf("ambiguous counters measured: %v", out.Units)
		}
	}
	// Exact-key maps keep case-distinct ids: questions "A" and "a" are two answers.
	input := &contract.DecisionInput{State: "s", Questions: []contract.DecisionQuestion{{ID: "A", Kind: "noul", Question: "?"}, {ID: "a", Kind: "noul", Question: "?"}}}
	body := `{"model":"jev-1.13.0","answers":{"A":{"type":"noul","noul":0.25},"a":{"type":"noul","noul":0.75}},"usage":{"input_tokens":1,"output_tokens":1}}`
	out, err := a.readDecisions(strings.NewReader(body), &provider.Call{Route: route, Decisions: input})
	if err != nil || *out.Decisions[0].Probability != 0.25 || *out.Decisions[1].Probability != 0.75 {
		t.Fatalf("case-distinct ids conflated: %v", err)
	}
}

func TestDecisionsFinalBodyBudgets(t *testing.T) {
	// Oxy measures this input at total 27941, gateway 32037: direct fits, the
	// gateway does not, because shared instructions repeat in every question.
	repeated := func(r *contract.Request) {
		v := strings.Repeat("i", 3000)
		r.Input.Decisions.Instructions = &v
		r.Input.Decisions.Questions = nil
		for k := range 9 {
			r.Input.Decisions.Questions = append(r.Input.Decisions.Questions, contract.DecisionQuestion{ID: "q" + string(rune('0'+k)), Kind: "noul", Question: "Q" + string(rune('0'+k))})
		}
	}
	for _, slug := range []contract.ProviderSlug{"typesafe", "openrouter"} {
		req, route := decisionFixture(slug)
		repeated(req)
		a := &Adapter{config: Config{Provider: slug, BaseURL: "https://example.invalid/v1"}, decisions: decisionReview{true, true, true, true, true}}
		call, err := a.Translate(req, route)
		if slug == "typesafe" && (err != nil || len(call.Body) > contract.DecisionTotalBudget) {
			t.Fatalf("direct positive control: %v", err)
		}
		var refusal provider.ErrUnsupported
		if slug == "openrouter" && (!errors.As(err, &refusal) || refusal.Code != contract.CodeRequestTooLarge) {
			t.Fatalf("gateway bound not enforced: %v", err)
		}
		// One question fewer fits the gateway, policy included.
		req.Input.Decisions.Questions = req.Input.Decisions.Questions[:8]
		call, err = a.Translate(req, route)
		if err != nil || len(call.Body) > contract.DecisionGatewayBudget {
			t.Fatalf("%s: fitting body refused: %v", slug, err)
		}
	}
	// Escaping counts: 5400 '<' are 32400 bytes on the wire.
	req, route := decisionFixture("typesafe")
	req.Input.Decisions.State = strings.Repeat("<", 5400)
	a := &Adapter{config: Config{Provider: "typesafe", BaseURL: "https://example.invalid/v1"}, decisions: decisionReview{true, true, true, true, true}}
	if _, err := a.Translate(req, route); err == nil {
		t.Fatal("escaped state measured by raw length")
	}
}

func TestDecisionsGatewayBodyBound(t *testing.T) {
	allowance, budget := contract.DecisionGatewayAllowance, contract.DecisionGatewayBudget
	for _, tc := range []struct {
		core, final int
		fits        bool
	}{
		{budget - allowance, budget, true},
		{budget - allowance + 1, budget - allowance + 1, false},
		{1000, 1000 + allowance, true},
		{1000, 1000 + allowance + 1, false},
	} {
		if gatewayBodyFits(tc.core, tc.final) != tc.fits {
			t.Errorf("core %d final %d: fits want %v", tc.core, tc.final, tc.fits)
		}
	}
}

// executeDecisions runs one decisions request through the real executor with
// TWO authorized routes and production retry policy, against one fake upstream.
func executeDecisions(t *testing.T, timeout time.Duration, handler http.HandlerFunc) (kaana.Result, int32) {
	t.Helper()
	var calls atomic.Int32
	req, route := decisionFixture("typesafe")
	a := decisionAdapter(t, "typesafe", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		handler(w, r)
	})
	a.decisions = decisionReview{true, true, true, true, true}
	a.decisionTimeout = timeout
	registry, err := provider.NewRegistry(a)
	if err != nil {
		t.Fatal(err)
	}
	second := route
	second.DeploymentID = "dep_decisions_second"
	deployments := []any{}
	for _, r := range []provider.Route{route, second} {
		deployments = append(deployments, map[string]any{"deploymentId": r.DeploymentID, "provider": r.Provider, "modelReference": r.ModelReference, "upstreamModelId": r.UpstreamModelID, "regions": []string{}, "current": true})
	}
	req.AuthorizedRoutes = append(req.AuthorizedRoutes, contract.AuthorizedRoute{Provider: second.Provider, DeploymentID: second.DeploymentID, ModelReference: second.ModelReference, Regions: []contract.Region{}, Substitution: contract.SubstitutionSameModel})
	path := filepath.Join(t.TempDir(), "inventory.json")
	document := map[string]any{"snapshotId": "synthetic-decisions", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": deployments}
	if err := os.WriteFile(path, []byte(mustJSON(t, document)), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := kaana.NewExecutor(kaana.Config{Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	result := executor.Execute(context.Background(), req, func(contract.StreamEvent) error { return nil })
	return result, calls.Load()
}

func TestDecisionsNeverMoveAfterDispatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		handler http.HandlerFunc
		code    contract.ErrorCode
		units   bool
	}{
		{"transport-cut", 0, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, contract.CodeProviderError, false},
		{"adapter-deadline", 50 * time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}, contract.CodeProviderTimeout, false},
		{"server-error", 0, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"synthetic"}}`)
		}, contract.CodeProviderError, false},
		{"malformed-body", 0, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":`)
		}, contract.CodeProviderError, false},
		{"invalid-answers-measured", 0, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Replace(decisionResponse("jev-1.13.0"), `"noul":0`, `"noul":7`, 1))
		}, contract.CodeProviderError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, calls := executeDecisions(t, tc.timeout, tc.handler)
			if calls != 1 {
				t.Fatalf("upstream received %d requests; possibly-accepted work was re-sent", calls)
			}
			if result.Failure == nil || result.Decisions != nil || result.Failure.Code != tc.code {
				t.Fatalf("failure = %+v", result.Failure)
			}
			// Settled exactly once, as the one attempt that ran, on the first route.
			if result.Report == nil || len(result.UpstreamCost.Attempts) != 1 || result.Report.RouteSwitches != 0 || result.Report.DeploymentID != "dep_decisions" || result.Report.Outcome == contract.OutcomeCompleted {
				t.Fatalf("settlement = %+v attempts=%d", result.Report, len(result.UpstreamCost.Attempts))
			}
			if measured := len(result.Report.Units) > 0; measured != tc.units {
				t.Fatalf("units = %v, measured want %v", result.Report.Units, tc.units)
			}
			if tc.units && result.Report.Units[0].Quantity != 12 {
				t.Fatalf("measured units lost: %v", result.Report.Units)
			}
		})
	}
}

func TestDecisionsSuccessThroughExecutorControl(t *testing.T) {
	result, calls := executeDecisions(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, decisionResponse("jev-1.13.0"))
	})
	if calls != 1 || result.Failure != nil || result.Decisions == nil || result.Report.Outcome != contract.OutcomeCompleted || len(result.UpstreamCost.Attempts) != 1 {
		t.Fatalf("positive control: calls=%d %+v", calls, result.Failure)
	}
}

// cancelOnEOF is a body whose read reaches EOF and then lets the deadline land
// before the adapter looks at its context: the race the adapter must survive.
type cancelOnEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnEOF) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.cancel()
	}
	return n, err
}

func TestDecisionsCompletedResultSurvivesLateDeadline(t *testing.T) {
	for _, valid := range []bool{true, false} {
		req, route := decisionFixture("typesafe")
		ctx, cancel := context.WithCancel(context.Background())
		body := decisionResponse(route.UpstreamModelID)
		if !valid {
			body = body[:len(body)-10]
		}
		a, err := New(Config{Provider: "typesafe", BaseURL: "https://api.typesafe.ai/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: cancelOnEOF{io.NopCloser(strings.NewReader(body)), cancel}, Request: r}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		a.decisions = decisionReview{true, true, true, true, true}
		call, err := a.Translate(req, route)
		if err != nil {
			t.Fatal(err)
		}
		out, err := a.Stream(ctx, call, nil, nil)
		if ctx.Err() == nil {
			t.Fatal("the late cancellation never landed")
		}
		if valid && (err != nil || len(out.Decisions) != 3) {
			t.Fatalf("completed answer discarded by a late deadline: %v", err)
		}
		if !valid && !errors.Is(err, context.Canceled) {
			t.Fatalf("a cut body must report the cancellation: %v", err)
		}
	}
}
