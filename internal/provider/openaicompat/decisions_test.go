package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func decisionFixture(slug contract.ProviderSlug) (*contract.Request, provider.Route) {
	ref := contract.ModelReference("typesafe/jev-1.13@2026-09-17")
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
	route := provider.Route{Provider: slug, DeploymentID: "dep_decisions", ModelReference: ref, UpstreamModelID: "jev-1.13.0"}
	if slug == "openrouter" {
		route.UpstreamModelID = "typesafe/jev-1.13-20260917"
	}
	req.AuthorizedRoutes = []contract.AuthorizedRoute{{Provider: slug, DeploymentID: route.DeploymentID, ModelReference: ref, Regions: []contract.Region{}, Substitution: contract.SubstitutionSameModel}}
	return req, route
}

func decisionResponse(model string) string {
	return `{"model":"` + model + `","id":"synthetic-generation","provider":"TypeSafe","answers":{"noul":{"type":"noul","noul":0},"score":{"type":"score","score":0.75,"confidence":0.5,"probabilities":{"1":0.75,"0":0.25}},"choice":{"type":"choice","choice":"b","confidence":0.5,"probabilities":{"b":0.75,"a":0.25}}},"usage":{"input_tokens":12,"output_tokens":4,"cost":0.000000000123}}`
}

func decisionAdapter(t *testing.T, slug contract.ProviderSlug, handler http.HandlerFunc) *Adapter {
	t.Helper()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)
	base := "https://api.typesafe.ai/v1"
	if slug == "openrouter" {
		base = "https://openrouter.ai/api/v1"
	}
	a, err := New(Config{Provider: slug, BaseURL: base, Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: identityBoundFakeClient(t, fake.URL)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDecisionsRealWireConformance(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"typesafe", "openrouter"} {
		t.Run(string(slug), func(t *testing.T) {
			req, route := decisionFixture(slug)
			var calls atomic.Int32
			a := decisionAdapter(t, slug, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				path := "/v1/systemone"
				if slug == "openrouter" {
					path = "/api/v1/systemone"
				}
				if r.URL.Path != path || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+fakeAPIKey {
					t.Error("wrong method, path or authentication")
				}
				var body systemOneRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != route.UpstreamModelID || len(body.Questions) != 3 || body.State != req.Input.Decisions.State {
					t.Error("wire input differs")
				}
				if slug == "openrouter" {
					p := body.Provider
					if p == nil || len(p.Only) != 1 || p.Only[0] != "TypeSafe" || len(p.Order) != 1 || p.Order[0] != "TypeSafe" || p.Ignore == nil || len(p.Ignore) != 0 || p.AllowFallbacks || !p.ZDR || p.DataCollection != "deny" || !p.RequireParameters || p.MaxPrice.Prompt != 0 || p.MaxPrice.Completion != 0 {
						t.Errorf("policy is not closed: %+v", p)
					}
				} else if body.Provider != nil {
					t.Error("direct call contains gateway policy")
				}
				_, _ = io.WriteString(w, decisionResponse(route.UpstreamModelID))
			})
			if _, err := a.Translate(req, route); err == nil {
				t.Fatal("ordinary credentials enabled dormant route")
			}
			// The positive control affirms reviews only on this synthetic adapter.
			a.decisions = decisionReview{true, true, true, true, true}
			call, err := a.Translate(req, route)
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.Stream(context.Background(), call, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Decisions) != 3 || out.Decisions[0].Probabilities[0] != 0.25 || *out.Decisions[2].Probability != 0 || out.ProviderReportedCost == nil || out.ProviderReportedCost.Amount != 123 || len(out.Units) != 3 || out.KeyID == "" {
				t.Fatalf("incorrect normalization: %+v", out)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d", calls.Load())
			}
			reviews := []decisionReview{{false, true, true, true, true}, {true, false, true, true, true}, {true, true, false, true, true}, {true, true, true, false, true}, {true, true, true, true, false}}
			for _, review := range reviews {
				a.decisions = review
				if _, err := a.Translate(req, route); err == nil {
					t.Error("missing review accepted")
				}
				if _, err := a.Stream(context.Background(), call, nil, nil); err == nil {
					t.Error("execution bypassed review")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("closed gates sent upstream work")
			}
		})
	}
}

func TestDecisionsMalformedAnswersPreserveMeasuredWork(t *testing.T) {
	req, route := decisionFixture("openrouter")
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

func TestDecisionsRefusalsAndCancellation(t *testing.T) {
	req, route := decisionFixture("typesafe")
	var calls atomic.Int32
	a := decisionAdapter(t, "typesafe", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
			_, _ = io.WriteString(w, decisionResponse(route.UpstreamModelID))
		}
	})
	a.decisions = decisionReview{true, true, true, true, true}
	for _, mutate := range []func(*contract.Request){
		func(r *contract.Request) { r.Stream = true },
		func(r *contract.Request) { v := strings.Repeat("x", 22000); r.Input.Decisions.Instructions = &v },
		func(r *contract.Request) { r.Modality = contract.ModalityAudio },
		func(r *contract.Request) { v := 0.5; r.Sampling.Temperature = &v },
		func(r *contract.Request) { v := contract.DecisionEffort("high"); r.Input.Decisions.Effort = &v },
	} {
		r, _ := decisionFixture("typesafe")
		mutate(r)
		if _, err := a.Translate(r, route); err == nil {
			t.Error("unsupported request accepted")
		}
	}
	alias := route
	alias.UpstreamModelID = "jev-latest"
	if _, err := a.Translate(req, alias); err == nil {
		t.Error("alias accepted")
	}
	call, err := a.Translate(req, route)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Stream(ctx, call, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("refusals sent a request")
	}
	if _, err := a.Stream(context.Background(), call, nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("uncancelled control did not complete")
	}
}

func TestDecisionsEndpointIdentityCannotBeBorrowed(t *testing.T) {
	for _, config := range []Config{{Provider: "typesafe", BaseURL: "https://openrouter.ai/api/v1"}, {Provider: "custom", BaseURL: "https://api.typesafe.ai/v1"}, {Provider: "openrouter", BaseURL: "https://openrouter.ai/api/alpha"}, {Provider: "typesafe", BaseURL: "https://api.typesafe.ai/v1/"}} {
		if _, err := New(config); err == nil {
			t.Fatalf("accepted %+v", config)
		}
	}
}

func TestDecisionsInFlightCancellationAndControl(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "cancelled"}[cancelled], func(t *testing.T) {
			started, release, disconnected := make(chan struct{}), make(chan struct{}), make(chan struct{})
			req, route := decisionFixture("typesafe")
			a := decisionAdapter(t, "typesafe", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
					close(disconnected)
				case <-release:
					_, _ = io.WriteString(w, decisionResponse(route.UpstreamModelID))
				}
			})
			a.decisions = decisionReview{true, true, true, true, true}
			call, err := a.Translate(req, route)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := a.Stream(ctx, call, nil, nil); result <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upstream never started")
			}
			if cancelled {
				cancel()
			} else {
				close(release)
			}
			select {
			case err := <-result:
				if cancelled && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
				if !cancelled && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("call did not terminate")
			}
			if cancelled {
				select {
				case <-disconnected:
				case <-time.After(time.Second):
					t.Fatal("upstream context was not cancelled")
				}
			} else {
				select {
				case <-disconnected:
					t.Fatal("positive control disconnected")
				default:
				}
			}
		})
	}
}

func TestDecisionsCredentialVerdicts(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"typesafe", "openrouter"} {
		for _, tc := range []struct {
			status int
			kind   string
			want   contract.ErrorCode
		}{
			{402, "billing_error", contract.CodeProviderBillingRefused},
			{429, "insufficient_quota", contract.CodeProviderBillingRefused},
			{429, "rate_limit_error", contract.CodeRateLimited},
			{401, "authentication_error", contract.CodeProviderCredentialInvalid},
		} {
			t.Run(string(slug)+"/"+tc.kind, func(t *testing.T) {
				req, route := decisionFixture(slug)
				a := decisionAdapter(t, slug, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": tc.kind, "message": "synthetic diagnostic echoed " + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}})
				})
				a.decisions = decisionReview{true, true, true, true, true}
				call, err := a.Translate(req, route)
				if err != nil {
					t.Fatal(err)
				}
				_, err = a.Stream(context.Background(), call, nil, nil)
				var failure provider.ErrUpstream
				if !errors.As(err, &failure) || failure.Code != tc.want {
					t.Fatalf("wrong verdict: %v", err)
				}
				text := mustJSON(t, failure)
				if strings.Contains(text, fakeAPIKey) || !strings.Contains(text, "synthetic diagnostic") {
					t.Fatal("redaction lost diagnostic or exposed credential")
				}
			})
		}
	}
}

func TestDecisionsRealWireThroughExecutor(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"typesafe", "openrouter"} {
		t.Run(string(slug), func(t *testing.T) {
			req, route := decisionFixture(slug)
			a := decisionAdapter(t, slug, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, decisionResponse(route.UpstreamModelID))
			})
			a.decisions = decisionReview{true, true, true, true, true}
			registry, err := provider.NewRegistry(a)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "inventory.json")
			document := map[string]any{"snapshotId": "synthetic-decisions", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []any{map[string]any{"deploymentId": route.DeploymentID, "provider": slug, "modelReference": route.ModelReference, "upstreamModelId": route.UpstreamModelID, "regions": []string{}, "current": true}}}
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
			if result.Failure != nil || result.Decisions == nil || result.Report == nil {
				t.Fatalf("execution failed: %+v", result)
			}
			if result.Decisions.Model != route.ModelReference || result.Report.ServingProvider != slug || result.Report.DeploymentID != route.DeploymentID || result.Report.Outcome != contract.OutcomeCompleted {
				t.Fatal("signed identity or settlement changed")
			}
			if len(result.UpstreamCost.Attempts) != 1 {
				t.Fatal("missing provider cost attempt")
			}
			wire := mustJSON(t, result.Decisions)
			if strings.Contains(wire, "cost") || strings.Contains(wire, "synthetic-generation") || strings.Contains(wire, "synthetic state") {
				t.Fatal("operator cost or content escaped")
			}
		})
	}
}
