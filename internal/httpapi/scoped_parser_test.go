package httpapi

import "testing"

func TestScopedNegotiationParserStrict(t *testing.T) {
	for _, bad := range []string{`{"scopedExecutionContractVersion":null}`, `{"scopedExecutionContractVersion":"3.5.0"}`, `{"scopedExecutionContractVersion":"3.6.0","scopedExecutionContractVersion":"3.6.0"}`, `{"scopedExecutionContractVersion":"3.6.0","scope":{}}`} {
		if _, err := parseDeploymentDescriptorQuery([]byte(bad)); err == nil {
			t.Fatal("bad negotiation admitted", bad)
		}
	}
}
