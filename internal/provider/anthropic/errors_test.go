package anthropic

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Pensara/internal/contract"
	"github.com/OxyHQ/Pensara/internal/provider"
)

// TestEveryFailureThisProviderNamesMapsOntoTheClosedVocabulary is the error
// table, and the column that matters is the last one.
//
// `attributable` decides whether a failure counts against the DEPLOYMENT: it is
// what opens a circuit breaker and what failover reads. Getting it wrong in one
// direction leaves a broken route in rotation failing every request sent to it;
// in the other, one customer's malformed traffic takes a healthy route out of
// rotation for everybody.
func TestEveryFailureThisProviderNamesMapsOntoTheClosedVocabulary(t *testing.T) {
	adapter := newTestAdapter(t)

	for name, testCase := range map[string]struct {
		kind         string
		status       int
		code         contract.ErrorCode
		category     contract.UpstreamErrorCategory
		retryable    bool
		attributable bool
	}{
		"a burst rate limit": {
			kind: errorRateLimit, status: http.StatusTooManyRequests,
			code: contract.CodeRateLimited, category: contract.UpstreamRateLimit,
			retryable: true, attributable: true,
		},
		// The pair this provider makes obvious and an OpenAI-compatible one does
		// not: an exhausted account arrives on a status no rate limit uses, so
		// the two are never confusable here — and are still classified by TYPE,
		// because that is what the mid-stream case has and a status is not.
		// `provider_billing_refused` rather than `quota_exceeded`: the account
		// at fault is the platform's with this provider, and the customer's own
		// ceiling is a different failure with a different remedy.
		"the platform's own account cannot be billed": {
			kind: errorBilling, status: http.StatusPaymentRequired,
			code: contract.CodeProviderBillingRefused, category: contract.UpstreamQuota,
			retryable: false, attributable: true,
		},
		// Non-retryable AND attributable, which is the pairing that makes this
		// row safe: the client stops retrying a request that cannot succeed,
		// the breaker still takes the route out of rotation, and a same-model
		// failover to a deployment holding a different credential is still
		// allowed.
		"the platform's credential was refused": {
			kind: errorAuthentication, status: http.StatusUnauthorized,
			code: contract.CodeProviderCredentialInvalid, category: contract.UpstreamAuthentication,
			retryable: false, attributable: true,
		},
		"the platform's credential lacks permission": {
			kind: errorPermission, status: http.StatusForbidden,
			code: contract.CodeProviderCredentialInvalid, category: contract.UpstreamAuthentication,
			retryable: false, attributable: true,
		},
		"the model this route names is gone": {
			kind: errorNotFound, status: http.StatusNotFound,
			code: contract.CodeModelNotFound, category: contract.UpstreamInvalidReq,
			retryable: false, attributable: false,
		},
		"the request is too large": {
			kind: errorRequestTooLarge, status: http.StatusRequestEntityTooLarge,
			code: contract.CodeRequestTooLarge, category: contract.UpstreamInvalidReq,
			retryable: false, attributable: false,
		},
		"the provider timed out": {
			kind: errorTimeout, status: http.StatusGatewayTimeout,
			code: contract.CodeProviderTimeout, category: contract.UpstreamTimeout,
			retryable: true, attributable: true,
		},
		// 529 is this provider's own status and is above 500, so an adapter
		// classifying by status alone reports it as an internal server error.
		// It is neither: it is the one failure that says "come back in a moment"
		// rather than "something broke".
		"the provider is overloaded": {
			kind: errorOverloaded, status: 529,
			code: contract.CodeProviderOverloaded, category: contract.UpstreamOverloaded,
			retryable: true, attributable: true,
		},
		"the provider failed internally": {
			kind: errorAPI, status: http.StatusInternalServerError,
			code: contract.CodeProviderError, category: contract.UpstreamServerError,
			retryable: true, attributable: true,
		},
		"the request was malformed": {
			kind: errorInvalidRequest, status: http.StatusBadRequest,
			code: contract.CodeInvalidRequest, category: contract.UpstreamInvalidReq,
			retryable: false, attributable: false,
		},
		// The provider's versioning policy says the type values may grow. An
		// unknown one with no status at all is the mid-stream case, and it must
		// not be blamed on the deployment: nobody can say whether the same
		// request would fail anywhere else.
		"a type this build has never seen, mid-stream": {
			kind: "some_future_error", status: 0,
			code: contract.CodeProviderError, category: contract.UpstreamUnknown,
			retryable: true, attributable: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			failure := adapter.classify(errorDetail{Type: testCase.kind, Message: "upstream said so"}, testCase.status, leasedKey(t, adapter))

			if failure.Code != testCase.code {
				t.Errorf("code is %q, expected %q", failure.Code, testCase.code)
			}
			if failure.Category != testCase.category {
				t.Errorf("category is %q, expected %q", failure.Category, testCase.category)
			}
			if failure.Code.Retryable() != testCase.retryable {
				t.Errorf("%q is retryable=%t, expected %t", failure.Code, failure.Code.Retryable(), testCase.retryable)
			}
			if got := provider.AttributableCategory(failure.Category); got != testCase.attributable {
				t.Errorf("%q is attributable to the deployment=%t, expected %t", failure.Category, got, testCase.attributable)
			}
			if failure.Passthrough == nil || failure.Passthrough.Provider != Slug {
				t.Error("the failure does not name the provider it came from")
			}
			if failure.Passthrough.Code == nil || *failure.Passthrough.Code != testCase.kind {
				t.Errorf("the passthrough does not carry the upstream's own code %q", testCase.kind)
			}
		})
	}
}

// TestARefusedPlatformCredentialDoesNotBlameTheCustomerAndIsNotRetried pins the
// reading behind the 401 row above, which is the one an adapter author is most
// likely to get backwards.
//
// The credential this provider refused is RELAY's, and it is the case
// `provider_credential_invalid` was added to the contract for
// (OxyHQ/oxy#1019): `authentication_failed` would send a customer to rotate
// their own key, and `provider_error` would send every client into a retry loop
// against a request that cannot succeed until an operator rotates ours.
func TestARefusedPlatformCredentialDoesNotBlameTheCustomerAndIsNotRetried(t *testing.T) {
	adapterUnderTest := newTestAdapter(t)
	failure := adapterUnderTest.classify(errorDetail{Type: errorAuthentication, Message: "invalid x-api-key"}, http.StatusUnauthorized, leasedKey(t, adapterUnderTest))

	if failure.Code != contract.CodeProviderCredentialInvalid {
		t.Errorf("a refused platform credential is reported as %q", failure.Code)
	}
	if failure.Code == contract.CodeAuthenticationFailed {
		t.Fatal("a refused PLATFORM credential was reported as the customer's authentication failing")
	}
	if failure.Code.Retryable() {
		t.Error("a refused platform credential is retryable, so clients will retry until an operator notices")
	}
	if !provider.AttributableCategory(failure.Category) {
		t.Error("a refused platform credential is not attributable to the deployment, so no breaker will ever take that route out of rotation")
	}
	if !strings.Contains(failure.Detail, "platform") {
		t.Errorf("the detail does not say whose credential was refused: %q", failure.Detail)
	}
}

// TestRetryAfterIsCarriedOnlyWhereTheContractAllowsIt covers the pairing the
// contract rejects outright: a retry hint on a code that can never be retried.
func TestRetryAfterIsCarriedOnlyWhereTheContractAllowsIt(t *testing.T) {
	adapter := newTestAdapter(t)

	for name, testCase := range map[string]struct {
		kind   string
		status int
		want   int
	}{
		"a rate limit carries the provider's own hint": {kind: errorRateLimit, status: http.StatusTooManyRequests, want: 2000},
		"an exhausted account carries none":            {kind: errorBilling, status: http.StatusPaymentRequired, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: testCase.status,
				Header:     http.Header{"Retry-After": []string{"2"}},
				Body:       errorBodyOf(testCase.kind),
			}

			var upstream provider.ErrUpstream
			if !asUpstream(adapter.Refuse(response, leasedKey(t, adapter)), &upstream) {
				t.Fatal("the adapter did not classify the failure")
			}
			if upstream.RetryAfterMs != testCase.want {
				t.Errorf("retryAfterMs is %d, expected %d", upstream.RetryAfterMs, testCase.want)
			}
			if upstream.Passthrough.Status == nil || *upstream.Passthrough.Status != testCase.status {
				t.Error("the passthrough does not carry the upstream status")
			}
		})
	}
}

// TestUpstreamErrorTextLosesTheCredentialAndKeepsTheDiagnostic is the leak this
// provider's header name makes possible.
func TestUpstreamErrorTextLosesTheCredentialAndKeepsTheDiagnostic(t *testing.T) {
	echoed := "request rejected: headers were {x-api-key: " + fakeAPIKey + "}"
	echoingAdapter := newTestAdapter(t)
	failure := echoingAdapter.classify(errorDetail{Type: errorInvalidRequest, Message: echoed}, http.StatusBadRequest, leasedKey(t, echoingAdapter))

	if failure.Passthrough.Message == nil {
		t.Fatal("the upstream's message was dropped entirely")
	}
	if strings.Contains(*failure.Passthrough.Message, fakeAPIKey) {
		t.Fatalf("the credential reached the customer-visible passthrough: %q", *failure.Passthrough.Message)
	}
	if !strings.Contains(*failure.Passthrough.Message, "request rejected") {
		t.Errorf("the diagnostic was destroyed along with the credential: %q", *failure.Passthrough.Message)
	}
}

// errorBodyOf renders one of this provider's error envelopes as a response body.
func errorBodyOf(kind string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"` + kind + `","message":"upstream said so"},"request_id":"req_fake"}`))
}

func asUpstream(err error, target *provider.ErrUpstream) bool {
	return errors.As(err, target)
}

// TestStopReasonsMapOntoTheContractsFinishReasons pins the distinction this
// port asked the contract for and got: a model DECLINING to answer is a
// property of the answer, and a content filter is an upstream system removing
// one. `refusal` joined `inferenceFinishReasonSchema` in
// @oxyhq/contracts@0.29.0 (contract version 1.1.0); before it, this row had to
// report `content_filter` and say something that was not quite true.
func TestStopReasonsMapOntoTheContractsFinishReasons(t *testing.T) {
	for reason, want := range map[string]contract.FinishReason{
		"end_turn":                      contract.FinishStop,
		"stop_sequence":                 contract.FinishStop,
		"pause_turn":                    contract.FinishStop,
		"max_tokens":                    contract.FinishLength,
		"model_context_window_exceeded": contract.FinishLength,
		"tool_use":                      contract.FinishToolCalls,
		"refusal":                       contract.FinishRefusal,
		// The provider's versioning policy allows new stop reasons. An
		// unrecognised one is reported as a normal stop rather than guessed at:
		// inventing a category would put a wrong reason on a receipt.
		"some_future_reason": contract.FinishStop,
	} {
		t.Run(reason, func(t *testing.T) {
			if got := mapStopReason(reason); got != want {
				t.Errorf("%q became %q, expected %q", reason, got, want)
			}
		})
	}
	if mapStopReason("refusal") == contract.FinishContentFilter {
		t.Error("a model refusal is reported as a content filter, which says an upstream system removed the answer")
	}
}

// TestThisProviderDeclaresNoQuotaHeader is an exact count, not a floor.
//
// The Messages API documents no remaining-credits header on a messages
// response, and no live call has been made from this repository to discover
// one. This provider's exhaustion is therefore learned from its own
// `billing_error` refusal, which is the strongest signal there is — the
// provider, about the exact credential that was sent. A guess here would retire
// healthy keys, which is the failure a declared mapping exists to prevent.
func TestThisProviderDeclaresNoQuotaHeader(t *testing.T) {
	if len(quotaHeaders) != 0 {
		t.Errorf("this provider declares %d quota headers; this build verified none, so the count is 0 until one is", len(quotaHeaders))
	}
}
