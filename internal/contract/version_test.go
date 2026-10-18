package contract

import "testing"

func TestRequestEnvelopeVersionTransitionIsExact(t *testing.T) {
	if LegacyRequestEnvelopeVersion == RequestEnvelopeVersion {
		t.Fatal("the legacy and current request-envelope versions are indistinguishable")
	}

	for version := -1; version <= RequestEnvelopeVersion+3; version++ {
		want := version == LegacyRequestEnvelopeVersion || version == RequestEnvelopeVersion
		if got := SupportsRequestEnvelopeVersion(version); got != want {
			t.Errorf("SupportsRequestEnvelopeVersion(%d) = %t, want %t", version, got, want)
		}
	}
}
