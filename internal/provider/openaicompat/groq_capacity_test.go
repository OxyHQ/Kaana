package openaicompat

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestGroqTokenCapacityRefusalIsNotAnInvalidRequest(t *testing.T) {
	cases := []struct {
		name   string
		slug   contract.ProviderSlug
		status int
		body   string
		want   contract.ErrorCode
	}{
		{"token capacity", "groq", 413, `{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Request exceeds tokens per minute (TPM): Limit 8000, Requested 11305"}}`, contract.CodeRateLimited},
		{"generic payload", "groq", 413, `{"error":{"type":"invalid_request_error","code":"request_too_large"}}`, contract.CodeRequestTooLarge},
		{"context length", "groq", 413, `{"error":{"type":"tokens","code":"context_length_exceeded"}}`, contract.CodeRequestTooLarge},
		{"untyped payload", "groq", 413, `{}`, contract.CodeRequestTooLarge},
		{"another provider", "openai", 413, `{"error":{"type":"tokens","code":"rate_limit_exceeded"}}`, contract.CodeRequestTooLarge},
		{"non scalar code", "groq", 413, `{"error":{"type":"tokens","code":{"kind":"rate_limit_exceeded"}}}`, contract.CodeRequestTooLarge},
		{"malformed request", "groq", 400, `{"error":{"type":"tokens","code":"rate_limit_exceeded"}}`, contract.CodeInvalidRequest},
		{"message alone", "groq", 413, `{"error":{"message":"rate_limit_exceeded tokens per minute (TPM)"}}`, contract.CodeRequestTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(Config{Provider: tc.slug, BaseURL: "https://example.invalid", Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
			if err != nil {
				t.Fatal(err)
			}
			err = a.Refuse(&http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, provider.Key{})
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) {
				t.Fatalf("failure %T: %v", err, err)
			}
			if failure.Code != tc.want {
				t.Fatalf("code = %s, want %s", failure.Code, tc.want)
			}
			if tc.want == contract.CodeRateLimited {
				if failure.Category != contract.UpstreamRateLimit || !provider.AttributableCategory(failure.Category) {
					t.Fatalf("capacity did not permit authorized failover: %+v", failure)
				}
				if failure.RetryAfterMs != 2000 {
					t.Fatalf("retry delay = %v", failure.RetryAfterMs)
				}
			} else if provider.AttributableCategory(failure.Category) {
				t.Fatal("invalid payload became attributable/retryable")
			}
		})
	}
}
