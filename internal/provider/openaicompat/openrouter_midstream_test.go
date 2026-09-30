package openaicompat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// OpenRouter's mid-stream error carries no `type`: `code` is the numeric HTTP
// status and `metadata.error_type` the reason
// (https://openrouter.ai/docs/api/reference/errors-and-debugging). Production
// saw exactly this frame for openai/gpt-6-luna on 2026-09-30 and reported it as
// an unclassified failure, so the request neither retried nor failed over.
const openRouterRateLimitFrame = `data: {"error":{"code":429,"message":"openai/gpt-6-luna is temporarily rate-limited upstream. Please retry shortly.","metadata":{"error_type":"rate_limit_exceeded"}},"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}` + "\n\n"

func TestOpenRouterMidStreamRateLimitIsARateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(openRouterRateLimitFrame))
	}))
	t.Cleanup(upstream.Close)
	base := providerconfig.Known["openrouter"].BaseURL
	adapter, err := New(Config{Provider: "openrouter", BaseURL: base, HTTPClient: identityBoundFakeClient(t, upstream.URL), Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	call := &provider.Call{
		Route:  provider.Route{Provider: "openrouter", ModelReference: "openai/gpt-6-luna@2026-09-30", UpstreamModelID: "openai/gpt-6-luna"},
		Method: http.MethodPost, URL: base + "/chat/completions", Body: []byte(`{}`), Stream: true,
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
	var upstreamErr provider.ErrUpstream
	if !errors.As(streamErr, &upstreamErr) {
		t.Fatalf("the mid-stream failure was reported as %T: %v", streamErr, streamErr)
	}
	if upstreamErr.Code != contract.CodeRateLimited || upstreamErr.Category != contract.UpstreamRateLimit {
		t.Fatalf("an OpenRouter rate limit was classified %s/%s", upstreamErr.Code, upstreamErr.Category)
	}
	if !provider.DeploymentAttributable(streamErr) {
		t.Fatal("an upstream rate limit must let the request fail over to another authorized route")
	}
	if upstreamErr.Passthrough == nil || upstreamErr.Passthrough.Message == nil {
		t.Fatal("OpenRouter's own explanation was dropped")
	}
}

func TestOpenRouterMidStreamErrorsAreClassifiedByTheirOwnFields(t *testing.T) {
	adapter, err := New(Config{Provider: "openrouter", BaseURL: providerconfig.Known["openrouter"].BaseURL, Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	key, _ := adapter.credentials.Begin().Next(time.Now())
	metadata := func(kind string) *upstreamErrorMetadata { return &upstreamErrorMetadata{ErrorType: kind} }
	for name, testCase := range map[string]struct {
		reported upstreamError
		want     contract.ErrorCode
	}{
		"429 by code":                   {upstreamError{Code: float64(429)}, contract.CodeRateLimited},
		"rate limit by error_type":      {upstreamError{Code: float64(400), Metadata: metadata("rate_limit_exceeded")}, contract.CodeRateLimited},
		"402 is the platform's billing": {upstreamError{Code: float64(402)}, contract.CodeProviderBillingRefused},
		"502 is a server failure":       {upstreamError{Code: float64(502)}, contract.CodeProviderError},
		"503 overloaded":                {upstreamError{Code: float64(503), Metadata: metadata("provider_overloaded")}, contract.CodeProviderError},
		"OpenAI's own type still wins":  {upstreamError{Type: "rate_limit_exceeded", Code: float64(500)}, contract.CodeRateLimited},
	} {
		var upstreamErr provider.ErrUpstream
		if !errors.As(adapter.streamFailure(testCase.reported, key), &upstreamErr) || upstreamErr.Code != testCase.want {
			t.Errorf("%s: classified %s, want %s", name, upstreamErr.Code, testCase.want)
		}
	}
	// The control: a frame naming nothing stays unclassified rather than guessed.
	var upstreamErr provider.ErrUpstream
	if !errors.As(adapter.streamFailure(upstreamError{Message: "something"}, key), &upstreamErr) ||
		upstreamErr.Category != contract.UpstreamUnknown {
		t.Errorf("an unnamed failure was classified %s/%s", upstreamErr.Code, upstreamErr.Category)
	}
}

// OpenRouter's 403 is a moderation block or a missing model permission, never
// the key (its 401 is). One such 403 used to retire the platform's only
// OpenRouter key and take every OpenRouter route out of rotation.
func TestOpenRouterForbiddenNeverRetiresTheKey(t *testing.T) {
	for name, testCase := range map[string]struct {
		body string
		want contract.ErrorCode
	}{
		"permission": {`{"error":{"code":403,"message":"This model is not available to your account"}}`, contract.CodePermissionDenied},
		"moderation": {`{"error":{"code":403,"message":"Input was flagged","metadata":{"reasons":["harassment"],"flagged_input":"…"}}}`, contract.CodeUpstreamContentFiltered},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(testCase.body))
			}))
			t.Cleanup(upstream.Close)
			base := providerconfig.Known["openrouter"].BaseURL
			adapter, err := New(Config{Provider: "openrouter", BaseURL: base, HTTPClient: identityBoundFakeClient(t, upstream.URL), Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
			if err != nil {
				t.Fatalf("building the adapter: %v", err)
			}
			call := &provider.Call{
				Route:  provider.Route{Provider: "openrouter", ModelReference: "meta/muse-spark-1.1@2026-09-01", UpstreamModelID: "meta/muse-spark-1.1"},
				Method: http.MethodPost, URL: base + "/chat/completions", Body: []byte(`{}`), Stream: true,
				Header: http.Header{"Content-Type": []string{"application/json"}},
			}
			_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
			var upstreamErr provider.ErrUpstream
			if !errors.As(streamErr, &upstreamErr) || upstreamErr.Code != testCase.want {
				t.Fatalf("an OpenRouter 403 was classified %v", streamErr)
			}
			if provider.CredentialVerdictFor(streamErr) == provider.CredentialRejected {
				t.Fatal("an OpenRouter 403 still reads as a rejected key")
			}
			if usable := adapter.credentials.Projection(time.Now()).Usable; usable != 1 {
				t.Fatalf("the only OpenRouter key left rotation after a 403; usable=%d", usable)
			}
		})
	}

	// The control: OpenRouter's 401 is the refused credential and still retires it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"No auth credentials found"}}`))
	}))
	t.Cleanup(upstream.Close)
	base := providerconfig.Known["openrouter"].BaseURL
	adapter, err := New(Config{Provider: "openrouter", BaseURL: base, HTTPClient: identityBoundFakeClient(t, upstream.URL), Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	call := &provider.Call{
		Route:  provider.Route{Provider: "openrouter", ModelReference: "meta/muse-spark-1.1@2026-09-01", UpstreamModelID: "meta/muse-spark-1.1"},
		Method: http.MethodPost, URL: base + "/chat/completions", Body: []byte(`{}`), Stream: true,
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
	if provider.CredentialVerdictFor(streamErr) != provider.CredentialRejected {
		t.Fatalf("OpenRouter's 401 no longer retires the refused key: %v", streamErr)
	}
}
