package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Relay/internal/contract"
)

// TestRedactSecretCoversWhatTheContractsPatternDoesNot is the reason
// RedactSecret exists, stated as a test rather than as a comment.
//
// The contract's credential-shaped-text pattern was written against providers
// that authenticate with a bearer token. Against one that authenticates with a
// header of its own name, an echoed request matches the MARKER and not the
// VALUE — so redacting alone removes the thing that made the text look like a
// credential and leaves the credential.
func TestRedactSecretCoversWhatTheContractsPatternDoesNot(t *testing.T) {
	const secret = "relay-test-fake-credential-0000"
	echoed := "request rejected: headers were {x-api-key: " + secret + "}"

	// The control, and the whole argument: the contract's own redaction is not
	// enough here. If this assertion ever fails because the published pattern
	// grew a rule for this shape, RedactSecret is still correct and this test
	// should be rewritten around whatever it no longer covers — not deleted.
	if !strings.Contains(contract.SafeErrorText(echoed), secret) {
		t.Fatal("the contract's pattern now redacts a header-named credential's value, so this test no longer measures the gap it was written for")
	}

	safe := contract.SafeErrorText(RedactSecret(echoed, secret))
	if strings.Contains(safe, secret) {
		t.Errorf("the credential survived redaction: %q", safe)
	}
	if !strings.Contains(safe, "request rejected") {
		t.Errorf("the diagnostic was destroyed along with the credential: %q", safe)
	}
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
