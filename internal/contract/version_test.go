package contract

import "testing"

func TestRequestEnvelopeVersionTransitionIsExact(t *testing.T) {
	if LegacyRequestEnvelopeVersion == RequestEnvelopeVersion {
		t.Fatal("the legacy and current request-envelope versions are indistinguishable")
	}

	for _, version := range []int{LegacyRequestEnvelopeVersion, RequestEnvelopeVersion} {
		if !SupportsRequestEnvelopeVersion(version) {
			t.Errorf("declared request-envelope version %d is not supported", version)
		}
	}

	for _, version := range []int{0, RequestEnvelopeVersion + 1} {
		if SupportsRequestEnvelopeVersion(version) {
			t.Errorf("undeclared request-envelope version %d is supported", version)
		}
	}
}
