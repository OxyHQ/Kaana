package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// TestNoProviderDeclaresAQuotaHeaderThisBuildHasNotVerified is an exact count,
// not a floor.
//
// The mapping is empty because no provider served by this protocol documents a
// remaining-credits header on a chat-completions response, and no live provider
// call has been made from this repository to discover one. A plausible guess
// here is the precise failure the mapping exists to prevent: `x-ratelimit-
// remaining` reaches zero on a BURST limit at several of these providers, and
// mapping it onto remaining credits would retire every key the first time one
// of them throttled us.
//
// An entry arrives with a count to move and a verified source to name, rather
// than as a line somebody appended.
func TestNoProviderDeclaresAQuotaHeaderThisBuildHasNotVerified(t *testing.T) {
	if len(declaredQuotaHeaders) != 0 {
		t.Errorf("%d providers declare a quota-header mapping; this build verified none, so the count is 0 until one is",
			len(declaredQuotaHeaders))
	}
	if quotaHeadersFor("openai") != nil {
		t.Error("a provider with no declared mapping was given one")
	}
}

// TestAQuotaHeaderIsReadOnEveryResponse is the entrypoint check under the
// mapping above.
//
// The table being empty makes the mechanism inert in this build, and an inert
// mechanism and a broken one look identical from outside. So this test declares
// a mapping for the duration of one request and requires the adapter to have
// applied it — which is the only thing that distinguishes "no provider declares
// a header" from "declaring one would do nothing".
//
// The upstream failure here is a 503, classified `healthy` for the credential.
// The key is retired anyway, by the header, which is the point: the proactive
// signal is a separate source from the refusal and does not depend on it.
func TestAQuotaHeaderIsReadOnEveryResponse(t *testing.T) {
	// The mapping is package state, so this test must not run in parallel with
	// the count above it. Go runs tests in a package sequentially unless they
	// call t.Parallel, and none here does.
	declaredQuotaHeaders["openai"] = provider.QuotaHeaders{"x-fake-credits-remaining": provider.SignalRemainingCredits}
	t.Cleanup(func() { delete(declaredQuotaHeaders, "openai") })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Fake-Credits-Remaining", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"the engine is busy","type":"server_error"}}`))
	}))
	t.Cleanup(upstream.Close)

	adapter, err := New(Config{
		Provider:     "openai",
		BaseURL:      upstream.URL,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey, fakeSecondAPIKey}),
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}

	call := &provider.Call{
		Route:  provider.Route{Provider: "openai", ModelReference: "openai/test-model@2026-05-01", UpstreamModelID: "test-model"},
		Method: http.MethodPost,
		URL:    upstream.URL + "/chat/completions",
		Body:   []byte(`{}`),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	if _, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}); streamErr == nil {
		t.Fatal("a 503 was reported as a successful stream")
	}

	projection := adapter.credentials.Projection(time.Now())
	if projection.Usable != 1 {
		t.Fatalf("%d of 2 credentials are usable; the header reported zero remaining for the one that was used", projection.Usable)
	}
	if projection.Keys[0].State != string(provider.KeyExhausted) {
		t.Errorf("the key that was used reports %q, expected %q", projection.Keys[0].State, provider.KeyExhausted)
	}

	// The control: the same 503 with no credits header retires nothing. Without
	// it, "the key went out of rotation" is also what a build that retired on
	// any 5xx would report.
	// The mapping is read once, when the adapter is built, so the control has
	// to be built after it is withdrawn.
	delete(declaredQuotaHeaders, "openai")
	control, err := New(Config{Provider: "openai", BaseURL: upstream.URL, Declarations: provider.DeclareKeys([]string{fakeAPIKey, fakeSecondAPIKey})})
	if err != nil {
		t.Fatalf("building the control adapter: %v", err)
	}
	if _, streamErr := control.Stream(context.Background(), call, silentEmitter{}); streamErr == nil {
		t.Fatal("a 503 was reported as a successful stream")
	}
	if usable := control.credentials.Projection(time.Now()).Usable; usable != 2 {
		t.Errorf("%d of 2 credentials are usable after a 503 nobody declared a quota header for", usable)
	}
}

// silentEmitter satisfies provider.Emitter for the failure paths, which produce
// no events at all: a refusal that arrives before the response body is read has
// nothing to emit, and the framing those events would need is the executor's
// and is covered by the conformance suite.
type silentEmitter struct{}

func (silentEmitter) Start(contract.ModelReference, time.Time) error { return nil }
func (silentEmitter) Delta(int, contract.DeltaChannel, string) error { return nil }
func (silentEmitter) ToolCall(provider.ToolCallDelta) error          { return nil }
func (silentEmitter) Usage([]contract.UsageQuantity, contract.UsageSource) error {
	return nil
}

// TestAThrottleRotatesOnlyWhenTheKeysAreDeclaredSeparateAccounts drives the one
// branch of the walk that no conformance scenario reaches, through a real
// adapter and a real upstream.
//
// A provider rate limit belongs to an ACCOUNT, and a pool's keys may or may not
// share one. Kaana cannot tell, and the two guesses fail in opposite
// directions: rotating into a shared limit hammers a provider that has just
// asked for less traffic, and refusing to rotate off separate accounts fails a
// request the next key would have served. So the operator states which it is.
func TestAThrottleRotatesOnlyWhenTheKeysAreDeclaredSeparateAccounts(t *testing.T) {
	for name, testCase := range map[string]struct {
		separateAccounts bool
		wantRequests     int
		wantServed       bool
	}{
		"one account, shared limit":     {separateAccounts: false, wantRequests: 1, wantServed: false},
		"separate accounts, own limits": {separateAccounts: true, wantRequests: 2, wantServed: true},
	} {
		t.Run(name, func(t *testing.T) {
			var requests int
			var firstCredential string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				credential := r.Header.Get("Authorization")
				if firstCredential == "" {
					firstCredential = credential
				}
				if credential == firstCredential {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_exceeded"}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
					"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: [DONE]\n\n"))
			}))
			t.Cleanup(upstream.Close)

			adapter, err := New(Config{
				Provider:     "openai",
				BaseURL:      upstream.URL,
				Declarations: provider.DeclareKeys([]string{fakeAPIKey, fakeSecondAPIKey}),
				Keys:         provider.KeyPolicy{OnSeparateAccounts: testCase.separateAccounts},
			})
			if err != nil {
				t.Fatalf("building the adapter: %v", err)
			}

			call := &provider.Call{
				Route:  provider.Route{Provider: "openai", ModelReference: "openai/test-model@2026-05-01", UpstreamModelID: "test-model"},
				Method: http.MethodPost,
				URL:    upstream.URL + "/chat/completions",
				Body:   []byte(`{}`),
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Stream: true,
			}
			_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{})

			if served := streamErr == nil; served != testCase.wantServed {
				t.Errorf("the request was served: %v, expected %v (%v)", served, testCase.wantServed, streamErr)
			}
			if requests != testCase.wantRequests {
				t.Errorf("the upstream received %d requests, expected %d", requests, testCase.wantRequests)
			}
			// A throttle retires nothing either way: it clears by itself, and a
			// key taken out for one would be out for fifteen minutes because a
			// provider asked for one fewer request this second.
			if usable := adapter.credentials.Projection(time.Now()).Usable; usable != 2 {
				t.Errorf("%d of 2 credentials are usable after a throttle; a throttle is not a statement about a key", usable)
			}
		})
	}
}

// A 402 is the platform's own account refusing to be billed, so it must retire
// the key and let the next one serve.
//
// This case was missing entirely and a 402 fell to the classifier's default,
// where it became `invalid_request`. The verdict for an invalid request is
// "the request was at fault", so the key stayed in rotation and every
// subsequent call spent the same unfundable account again. It is not a
// hypothetical shape: measured 2026-08-18, Cerebras answers 402 with
// `param: quota` to every completion on the account this deployment holds, and
// OpenRouter answers it once an account is out of credit.
func TestAPaymentRefusalRetiresTheKeyAndTheNextOneServes(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		seen = append(seen, authorization)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(authorization, fakeAPIKey) {
			// The shape Cerebras really sends: a bare 402, no OpenAI error type.
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"payment required","param":"quota"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	adapter, err := New(Config{
		Provider:     "openai",
		BaseURL:      upstream.URL,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey, fakeSecondAPIKey}),
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}

	call := &provider.Call{
		Route:  provider.Route{Provider: "openai", ModelReference: "openai/test-model@2026-05-01", UpstreamModelID: "test-model"},
		Method: http.MethodPost,
		URL:    upstream.URL + "/chat/completions",
		Body:   []byte(`{"model":"test-model"}`),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	outcome, err := adapter.Stream(context.Background(), call, silentEmitter{})
	if err != nil {
		t.Fatalf("the second key did not serve the request: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("the upstream saw %d attempts; the walk did not move to the next key", len(seen))
	}
	if outcome.KeyID != "key-2" {
		t.Errorf("the served attempt reports key %q, want key-2", outcome.KeyID)
	}
}

// The negative control for the case above. Without it, a classifier that
// retired a key on ANY failure would pass it — and retiring a healthy key
// because one request was malformed empties a pool for no reason.
func TestAnInvalidRequestDoesNotRetireTheKey(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(upstream.Close)

	adapter, err := New(Config{
		Provider:     "openai",
		BaseURL:      upstream.URL,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey, fakeSecondAPIKey}),
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	call := &provider.Call{
		Route:  provider.Route{Provider: "openai", ModelReference: "openai/test-model@2026-05-01", UpstreamModelID: "test-model"},
		Method: http.MethodPost,
		URL:    upstream.URL + "/chat/completions",
		Body:   []byte(`{"model":"test-model"}`),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	if _, err := adapter.Stream(context.Background(), call, silentEmitter{}); err == nil {
		t.Fatal("a malformed request was reported as a success")
	}
	if attempts != 1 {
		t.Fatalf("the walk tried %d keys for a request fault; the pool must not be spent on one", attempts)
	}
}
