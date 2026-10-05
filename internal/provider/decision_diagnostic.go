package provider

import "strings"

// DecisionResponseDiagnostic projects only finite local validation reasons into
// operator logs. A prefix match alone is insufficient: upstream-controlled
// error text, suffixes and values must never become log attributes.
func DecisionResponseDiagnostic(detail string) string {
	const prefix = "invalid systemone response: "
	if !strings.HasPrefix(detail, prefix) {
		return ""
	}
	reason := strings.TrimPrefix(detail, prefix)
	switch reason {
	case "body_read", "response_json", "ambiguous_json", "trailing_json",
		"missing_questions", "model_identity", "usage_units", "answer_count",
		"provider_identity", "answer_identity", "answer_json", "answer_kind",
		"choice_probability_count", "choice_probability_key", "score_probability_count",
		"score_probability_key", "noul_fields", "reply_confidence", "choice_value",
		"choice_reply", "choice_maximum", "score_value", "score_reply", "noul_value",
		"probability_value", "probability_sum", "score_distribution", "answer_contract":
		return reason
	default:
		return ""
	}
}
