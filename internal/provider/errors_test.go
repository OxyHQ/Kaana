package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// TestRedactSecretIsStillTheOnlyControl answers the question the contract's own
// rewrite raises: `@oxyhq/contracts@0.29.0` closed the header-name hole this
// repository reported, so is an adapter-side redaction still needed?
//
// It is, for two separate reasons, and the second is a leak.
//
// The published refinement is a REFUSAL and says so: a producer whose message
// still looks like it carries a credential loses the message, not the
// credential. And it cannot see a credential with no marker, no issued-token
// prefix and no placeholder beside it — the contract states that limit outright,
// because refusing those bytes means refusing request ids.
//
// An adapter is holding the exact bytes it sent. It does not need a heuristic.
func TestRedactSecretIsStillTheOnlyControl(t *testing.T) {
	const secret = "kaana0test0fake0credential0value"

	t.Run("a shape the contract now recognises: the diagnostic survives", func(t *testing.T) {
		echoed := "request rejected: headers were {x-api-key: " + secret + "}"

		// Without the adapter's redaction the contract refuses the whole string,
		// so nothing leaks — and the customer is told nothing either.
		withheld := contract.SafeErrorText(echoed)
		if strings.Contains(withheld, secret) {
			t.Fatal("the contract's refusal let a marked credential through")
		}
		if strings.Contains(withheld, "request rejected") {
			t.Fatal("the refusal preserved the diagnostic, so this case no longer measures what it was written for")
		}

		// With it, the value is gone and the message still says what happened.
		safe := contract.SafeErrorText(RedactSecret(echoed, secret))
		if strings.Contains(safe, secret) {
			t.Errorf("the credential survived redaction: %q", safe)
		}
		if !strings.Contains(safe, "request rejected") {
			t.Errorf("the diagnostic was lost even though the secret was removable: %q", safe)
		}
	})

	t.Run("a shape the contract cannot see: the credential leaks without it", func(t *testing.T) {
		// No marker, no issued-token prefix, no placeholder beside it. The
		// contract accepts this string by design; internal/contract's fixture
		// table pins that against the published schema itself.
		echoed := "the key " + secret + " was not accepted"

		if !strings.Contains(contract.SafeErrorText(echoed), secret) {
			t.Fatal("the published pattern now recognises an unmarked credential; re-derive what it still cannot see rather than deleting this case")
		}
		if got := contract.SafeErrorText(RedactSecret(echoed, secret)); strings.Contains(got, secret) {
			t.Errorf("the credential reached the customer: %q", got)
		}
	})
}

func TestRedactSecretLeavesTextAloneWhenThereIsNoSecret(t *testing.T) {
	// Negative control: an adapter with no credential configured must not have
	// its error text mangled, or "nothing leaked" would be what a redaction
	// that eats everything also reports.
	const ordinary = "the model is overloaded; retry in 2s"
	if got := RedactSecret(ordinary, ""); got != ordinary {
		t.Errorf("empty secret changed the text:\n want %q\n got  %q", ordinary, got)
	}
}

func TestRetryAfterMsReadsBothFormsAndInventsNothing(t *testing.T) {
	for name, testCase := range map[string]struct {
		header http.Header
		want   int
	}{
		"seconds":     {header: http.Header{"Retry-After": []string{"2"}}, want: 2000},
		"absent":      {header: http.Header{}, want: 0},
		"unparseable": {header: http.Header{"Retry-After": []string{"soon"}}, want: 0},
		"past date":   {header: http.Header{"Retry-After": []string{"Wed, 21 Oct 2015 07:28:00 GMT"}}, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RetryAfterMs(testCase.header); got != testCase.want {
				t.Errorf("RetryAfterMs = %d, want %d", got, testCase.want)
			}
		})
	}
}

// TestARefusedPlatformCredentialIsNonRetryableAndStillAttributable pins the
// pairing that makes `provider_credential_invalid` safe to use.
//
// The two halves pull in opposite directions and both are needed. The CODE is
// non-retryable, because no client retry reaches the operator who has to rotate
// a key — that is the defect OxyHQ/oxy#1019 added it to fix. The CATEGORY is
// attributable, because the credential belongs to this deployment and another
// deployment holds a different one: the breaker must take this route out of
// rotation, and a same-model failover must still be allowed to try the other.
//
// An adapter reaching for a non-attributable category here would leave a route
// with a dead key in rotation, failing every request sent to it.
func TestARefusedPlatformCredentialIsNonRetryableAndStillAttributable(t *testing.T) {
	if contract.CodeProviderCredentialInvalid.Retryable() {
		t.Error("provider_credential_invalid is retryable, so every client will retry a request that cannot succeed until a human acts")
	}
	if !AttributableCategory(contract.UpstreamAuthentication) {
		t.Error("an authentication failure is not attributable to the deployment, so no breaker will ever take a route with a dead credential out of rotation")
	}
	// The control on the first assertion: a code that IS retryable, so
	// "non-retryable" cannot be what a broken Retryable() reports for everything.
	if !contract.CodeProviderOverloaded.Retryable() {
		t.Error("provider_overloaded is non-retryable, so the assertion above measures nothing")
	}
	// The control on the second: a category that is NOT attributable.
	if AttributableCategory(contract.UpstreamContentFilter) {
		t.Error("a content filter is attributable to the deployment, so the assertion above measures nothing")
	}
}
