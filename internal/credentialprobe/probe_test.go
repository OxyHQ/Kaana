package credentialprobe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func answer(status int, body string) roundTrip {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}
}

func tcSlug(slug string) contract.ProviderSlug { return contract.ProviderSlug(slug) }

// The bodies below are the ones each provider returned to a made-up key on
// 2026-11-17, verbatim apart from the request id.
const (
	anthropicInvalidKey  = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"},"request_id":"req_x"}`
	elevenLabsInvalidKey = `{"detail":{"type":"authentication_error","code":"unauthorized","message":"Invalid API key","status":"invalid_api_key","request_id":"x"}}`
)

func TestEachReviewedProviderSaysRejectedOnlyOnItsDocumentedInvalidKeyError(t *testing.T) {
	cases := []struct {
		name   string
		slug   string
		status int
		body   string
		want   credentialstore.LivenessVerdict
	}{
		{"anthropic invalid key", "anthropic", 401, anthropicInvalidKey, credentialstore.LivenessRejected},
		{"anthropic authenticated", "anthropic", 200, `{"data":[]}`, credentialstore.LivenessAccepted},
		{"anthropic 401 of another type", "anthropic", 401, `{"type":"error","error":{"type":"permission_error"}}`, credentialstore.LivenessUnknown},
		{"anthropic 401 without a body", "anthropic", 401, ``, credentialstore.LivenessUnknown},
		{"anthropic permission", "anthropic", 403, `{"type":"error","error":{"type":"permission_error"}}`, credentialstore.LivenessUnknown},
		{"anthropic billing (a funded key works)", "anthropic", 402, `{"type":"error","error":{"type":"billing_error"}}`, credentialstore.LivenessUnknown},
		{"anthropic throttle", "anthropic", 429, `{"type":"error","error":{"type":"rate_limit_error"}}`, credentialstore.LivenessUnknown},
		{"anthropic outage", "anthropic", 529, `{"type":"error","error":{"type":"overloaded_error"}}`, credentialstore.LivenessUnknown},
		{"elevenlabs invalid key", "elevenlabs", 401, elevenLabsInvalidKey, credentialstore.LivenessRejected},
		{"elevenlabs legacy invalid key", "elevenlabs", 401, `{"detail":{"status":"invalid_api_key","message":"Invalid API key"}}`, credentialstore.LivenessRejected},
		{"elevenlabs authenticated", "elevenlabs", 200, `[]`, credentialstore.LivenessAccepted},
		{"elevenlabs scoped key missing a permission", "elevenlabs", 401, `{"detail":{"type":"authentication_error","code":"unauthorized","status":"missing_permissions"}}`, credentialstore.LivenessUnknown},
		{"elevenlabs ip restriction", "elevenlabs", 403, `{"detail":{"type":"authorization_error","status":"invalid_api_key"}}`, credentialstore.LivenessUnknown},
		{"elevenlabs invalid status under another type", "elevenlabs", 401, `{"detail":{"type":"authorization_error","status":"invalid_api_key"}}`, credentialstore.LivenessUnknown},
		{"elevenlabs unauthenticated workspace", "elevenlabs", 404, `{"detail":{"status":"workspace_not_found"}}`, credentialstore.LivenessUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober, err := New(answer(tc.status, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := prober.Probe(context.Background(), tcSlug(tc.slug), []byte("sk-test"))
			if err != nil || got != tc.want {
				t.Fatalf("Probe = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestAProviderWithoutAReviewedProbeIsUnknownAndNotCalled(t *testing.T) {
	called := false
	prober, _ := New(roundTrip(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	}))
	for _, slug := range []string{"groq", "openrouter", "xai", "cerebras", "openai"} {
		got, err := prober.Probe(context.Background(), tcSlug(slug), []byte("sk-test"))
		if err != nil || got != credentialstore.LivenessUnknown {
			t.Fatalf("%s: Probe = %q, %v; want unknown", slug, got, err)
		}
	}
	if called {
		t.Fatal("a provider with no reviewed probe was sent the secret")
	}
}

func TestTheProbeSendsTheSecretOnceToTheFixedOriginWithTheProvidersHeader(t *testing.T) {
	for slug, want := range map[string]struct{ url, header string }{
		"anthropic":  {AnthropicEndpoint, "X-Api-Key"},
		"elevenlabs": {ElevenLabsEndpoint, "Xi-Api-Key"},
	} {
		calls := 0
		prober, _ := New(roundTrip(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Method != http.MethodGet || request.URL.String() != want.url {
				t.Errorf("%s: %s %s, want GET %s", slug, request.Method, request.URL, want.url)
			}
			if request.Header.Get(want.header) != "sk-test" || request.Header.Get("Authorization") != "" {
				t.Errorf("%s: the secret travelled in the wrong header", slug)
			}
			if slug == "anthropic" && request.Header.Get("Anthropic-Version") == "" {
				t.Error("anthropic: no anthropic-version header")
			}
			return answer(200, `{}`)(request)
		}))
		if _, err := prober.Probe(context.Background(), tcSlug(slug), []byte("sk-test")); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("%s: %d requests, want 1", slug, calls)
		}
	}
}

func TestATransportFailureIsUnknownAndNeverQuotesTheRequest(t *testing.T) {
	prober, _ := New(roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for x-api-key sk-test")
	}))
	got, err := prober.Probe(context.Background(), "anthropic", []byte("sk-test"))
	if got != credentialstore.LivenessUnknown || !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("Probe = %q, %v", got, err)
	}
	if strings.Contains(err.Error(), "sk-test") {
		t.Fatal("the error quotes the secret")
	}
}

func TestAnOversizedAnswerIsUnknown(t *testing.T) {
	prober, _ := New(answer(401, `{"type":"error","error":{"type":"authentication_error"},"pad":"`+strings.Repeat("a", MaxResponseBytes)+`"}`))
	got, err := prober.Probe(context.Background(), "anthropic", []byte("sk-test"))
	if got != credentialstore.LivenessUnknown || !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Probe = %q, %v", got, err)
	}
}

func TestAnInvalidHeaderSecretIsNeverSent(t *testing.T) {
	prober, _ := New(roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("sent")
		return nil, nil
	}))
	if _, err := prober.Probe(context.Background(), "anthropic", []byte("sk\nInjected: x")); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Probe error = %v", err)
	}
}

func TestARedirectIsNeverFollowed(t *testing.T) {
	calls := 0
	prober, _ := New(roundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://example.com/steal"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	got, _ := prober.Probe(context.Background(), "anthropic", []byte("sk-test"))
	if got != credentialstore.LivenessUnknown || calls != 1 {
		t.Fatalf("Probe = %q after %d requests; a redirect must not be followed", got, calls)
	}
}
