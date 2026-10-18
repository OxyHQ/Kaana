package openaicompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/conformance"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Both decisions adapters pass the same suite against fakes of their real
// wires: TypeSafe's Jev on OpenRouter's /systemone, and OpenAI's /decisions.
func TestDecisionsAdaptersConform(t *testing.T) {
	for _, wire := range []decisionsWire{systemOneWire(), openAIDecisionsWire()} {
		t.Run(wire.subject.Name, func(t *testing.T) {
			subject := wire.subject
			subject.APIKeys = []string{fakeAPIKey, fakeSecondAPIKey}
			subject.NewAdapter = func(t *testing.T, upstreamURL string) provider.Adapter {
				t.Helper()
				adapter, err := New(Config{
					Provider: subject.Provider, BaseURL: providerconfig.Known[subject.Provider].BaseURL,
					Declarations: provider.DeclareKeys(subject.APIKeys), HTTPClient: identityBoundFakeClient(t, upstreamURL),
				})
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			}
			subject.StartUpstream = func(t *testing.T, scenario conformance.DecisionsScenario) *conformance.DecisionsUpstream {
				return startDecisionsFake(t, wire, scenario)
			}
			conformance.RunDecisions(t, subject)
		})
	}
}

// decisionsWire is one provider's decisions wire: where it is posted, what
// the request must say, and the bodies the fake answers with.
type decisionsWire struct {
	subject                      conformance.DecisionsSubject
	path                         string
	checkRequest                 func(t *testing.T, body []byte)
	answered, malformed, refusal string
	refusalStatus                int
}

func systemOneWire() decisionsWire {
	const model = "typesafe/jev-1.13-20260917"
	return decisionsWire{
		subject: conformance.DecisionsSubject{Name: "openrouter-jev", Provider: "openrouter", ModelReference: "typesafe/jev-1.13@observed-2026-10-19", UpstreamModelID: model, InputTokens: 61},
		path:    "/api/v1/systemone",
		checkRequest: func(t *testing.T, body []byte) {
			var request struct {
				Model     string                     `json:"model"`
				Questions map[string]json.RawMessage `json:"questions"`
				Provider  struct {
					Only           []string `json:"only"`
					Order          []string `json:"order"`
					AllowFallbacks *bool    `json:"allow_fallbacks"`
					DataCollection string   `json:"data_collection"`
					ZDR            bool     `json:"zdr"`
					MaxPrice       struct {
						Prompt     json.Number `json:"prompt"`
						Completion json.Number `json:"completion"`
					} `json:"max_price"`
				} `json:"provider"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Errorf("systemone request: %v", err)
				return
			}
			p := request.Provider
			if request.Model != model || len(request.Questions) != 3 || len(p.Only) != 1 || p.Only[0] != "TypeSafe" || len(p.Order) != 1 || p.Order[0] != "TypeSafe" ||
				p.AllowFallbacks == nil || *p.AllowFallbacks || p.DataCollection != "deny" || !p.ZDR || !sameNumber(p.MaxPrice.Prompt, 0.1) || !sameNumber(p.MaxPrice.Completion, 0) {
				t.Errorf("the systemone request is not pinned to TypeSafe under its rate card: %s", body)
			}
		},
		answered: `{"model":"` + model + `","id":"gen-synthetic","provider":"TypeSafe","answers":{` +
			`"spam":{"type":"noul","noul":0.1},` +
			`"quality":{"type":"score","score":1.6,"confidence":0.6,"probabilities":{"2":0.7,"0":0.1,"1":0.2}},` +
			`"topic":{"type":"choice","choice":"pets","confidence":0.9,"probabilities":{"cars":0.2,"pets":0.8}}},` +
			`"usage":{"input_tokens":61,"output_tokens":0,"cost":0.0000061}}`,
		malformed: `{"model":"` + model + `","id":"gen-synthetic","provider":"TypeSafe","answers":{` +
			`"spam":{"type":"noul","noul":1.5},` +
			`"quality":{"type":"score","score":1.6,"confidence":0.6,"probabilities":{"2":0.7,"0":0.1,"1":0.2}},` +
			`"topic":{"type":"choice","choice":"pets","confidence":0.9,"probabilities":{"cars":0.2,"pets":0.8}}},` +
			`"usage":{"input_tokens":61,"output_tokens":0}}`,
		// OpenRouter's moderation refusal: a 403 naming its reasons, unbilled.
		refusal:       `{"error":{"code":403,"message":"Input was flagged","metadata":{"reasons":["synthetic"],"flagged_input":"..."}}}`,
		refusalStatus: http.StatusForbidden,
	}
}

func openAIDecisionsWire() decisionsWire {
	const model = "gpt-6-luna"
	return decisionsWire{
		subject: conformance.DecisionsSubject{Name: "openai-decisions", Provider: "openai-decisions", ModelReference: "openai/gpt-6-luna@observed-2026-10-19", UpstreamModelID: model, InputTokens: 58, RefusalInputTokens: 58},
		path:    "/v1/decisions",
		checkRequest: func(t *testing.T, body []byte) {
			var request openAIDecisionsRequest
			if err := json.Unmarshal(body, &request); err != nil {
				t.Errorf("decisions request: %v", err)
				return
			}
			q := request.Questions
			if request.Model != model || request.Input != conformance.DecisionsInput().State || len(q) != 3 ||
				q[0].Type != "choice" || q[0].Name != "topic" || len(q[0].Choices) != 2 || q[0].Choices[0] != (openAIDecisionChoice{Value: "pets", Description: "pets"}) ||
				q[1].Type != "score" || len(q[1].Levels) != 3 || q[1].Levels[2] != (openAIDecisionLevel{Label: "high", Description: "high"}) ||
				q[2].Type != "predicate" || q[2].Instructions != "Judge the post.\n\nIs it spam?" {
				t.Errorf("the decisions request is not the questions' translation: %s", body)
			}
		},
		// The score is printed rounded; the probabilities come in no order.
		answered: `{"id":"dec_synthetic","object":"decision","model":"` + model + `","answers":[` +
			`{"type":"predicate","name":"spam","probability":0.1},` +
			`{"type":"score","name":"quality","score":1.60,"confidence":0.6,"probabilities":[{"value":2,"label":"high","probability":0.7},{"value":0,"label":"low","probability":0.1},{"value":1,"label":"mid","probability":0.2}]},` +
			`{"type":"choice","name":"topic","choice":"pets","confidence":0.9,"probabilities":[{"value":"cars","probability":0.2},{"value":"pets","probability":0.8}]}],` +
			`"usage":{"input_tokens":58}}`,
		malformed: `{"id":"dec_synthetic","model":"` + model + `","answers":[` +
			`{"type":"predicate","name":"spam","probability":1.5},` +
			`{"type":"score","name":"quality","score":1.6,"confidence":0.6,"probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"mid","probability":0.2},{"value":2,"label":"high","probability":0.7}]},` +
			`{"type":"choice","name":"topic","choice":"pets","confidence":0.9,"probabilities":[{"value":"pets","probability":0.8},{"value":"cars","probability":0.2}]}],` +
			`"usage":{"input_tokens":58}}`,
		refusal: `{"id":"dec_synthetic","model":"` + model + `","answers":[` +
			`{"type":"refusal","name":"spam"},{"type":"refusal","name":"quality"},{"type":"refusal","name":"topic"}],` +
			`"usage":{"input_tokens":58}}`,
		refusalStatus: http.StatusOK,
	}
}

// startDecisionsFake serves one scenario of a decisions wire. Key-dependent
// scenarios refuse the first key and answer the second; every request is
// checked against the questions it must translate.
func startDecisionsFake(t *testing.T, wire decisionsWire, scenario conformance.DecisionsScenario) *conformance.DecisionsUpstream {
	t.Helper()
	var requests atomic.Int32
	var disconnected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != wire.path {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		wire.checkRequest(t, body)
		firstKey := r.Header.Get("Authorization") == "Bearer "+fakeAPIKey
		w.Header().Set("Content-Type", "application/json")
		switch {
		case scenario == conformance.DecisionsFirstKeyRejected && firstKey:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid key"}}`)
		case scenario == conformance.DecisionsFirstKeyUnpaid && firstKey:
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"error":{"type":"billing_error","message":"insufficient credits"}}`)
		case scenario == conformance.DecisionsRefused:
			w.WriteHeader(wire.refusalStatus)
			_, _ = io.WriteString(w, wire.refusal)
		case scenario == conformance.DecisionsMalformed:
			_, _ = io.WriteString(w, wire.malformed)
		case scenario == conformance.DecisionsCut:
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		case scenario == conformance.DecisionsHeld:
			<-r.Context().Done()
			disconnected.Store(true)
		default:
			_, _ = io.WriteString(w, wire.answered)
		}
	}))
	t.Cleanup(server.Close)
	return &conformance.DecisionsUpstream{
		URL:          server.URL,
		Requests:     func() int { return int(requests.Load()) },
		Disconnected: disconnected.Load,
	}
}

func sameNumber(number json.Number, want float64) bool {
	got, err := number.Float64()
	return err == nil && got == want
}
