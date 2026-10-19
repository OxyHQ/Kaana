package provider

import "testing"

// Only a reason from the closed set reaches an operator log; anything an
// upstream could have written, with or without the prefix, does not.
func TestDecisionResponseDiagnosticIsClosed(t *testing.T) {
	for detail, want := range map[string]string{
		InvalidDecisionResponse("score_distribution").Detail:                          "score_distribution",
		InvalidDecisionResponse("PRIVATE_UNRECOGNIZED_CANARY").Detail:                 "answer_contract",
		"PRIVATE_PROVIDER_SECRET_CANARY":                                              "",
		decisionResponsePrefix + "PRIVATE_PROVIDER_SECRET_CANARY":                     "",
		decisionResponsePrefix + "score_distribution PRIVATE_PROVIDER_SECRET_CANARY":  "",
		decisionResponsePrefix + "score_distribution\nPRIVATE_PROVIDER_SECRET_CANARY": "",
	} {
		if got := DecisionResponseDiagnostic(detail); got != want {
			t.Errorf("DecisionResponseDiagnostic(%q) = %q, want %q", detail, got, want)
		}
	}
}
