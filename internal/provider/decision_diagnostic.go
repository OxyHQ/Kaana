package provider

import (
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const decisionResponsePrefix = "invalid decisions response: "

// decisionResponseReasons are the finite reasons a decisions adapter refuses
// a provider's answer with: its own checks and every contract.DecisionAnswerError.
var decisionResponseReasons = map[string]bool{
	"body_read": true, "response_json": true, "ambiguous_json": true, "trailing_json": true,
	"missing_questions": true, "model_identity": true, "usage_units": true, "answer_count": true,
	"provider_identity": true, "answer_identity": true, "answer_json": true, "answer_kind": true,
	"choice_probability_count": true, "choice_probability_key": true, "score_probability_count": true,
	"score_probability_key": true, "noul_fields": true, "reply_confidence": true, "choice_value": true,
	"choice_reply": true, "choice_maximum": true, "score_value": true, "score_reply": true, "noul_value": true,
	"probability_value": true, "probability_sum": true, "score_distribution": true, "answer_contract": true,
}

// InvalidDecisionResponse is a provider's decisions answer Kaana refused: a
// provider_error of unknown category whose detail names one finite reason. A
// reason outside the closed set becomes `answer_contract`.
func InvalidDecisionResponse(reason string) ErrUpstream {
	if !decisionResponseReasons[reason] {
		reason = "answer_contract"
	}
	return ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: decisionResponsePrefix + reason}
}

// DecisionResponseDiagnostic projects only finite local validation reasons into
// operator logs. A prefix match alone is insufficient: upstream-controlled
// error text, suffixes and values must never become log attributes.
func DecisionResponseDiagnostic(detail string) string {
	reason, ok := strings.CutPrefix(detail, decisionResponsePrefix)
	if !ok || !decisionResponseReasons[reason] {
		return ""
	}
	return reason
}
