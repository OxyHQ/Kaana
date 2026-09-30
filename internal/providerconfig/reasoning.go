package providerconfig

import "github.com/OxyHQ/Kaana/internal/contract"

// ReasoningEfforts is the reasoning efforts one deployment's upstream accepts,
// where Kaana's adapter holds a reviewed per-model statement of them.
//
// stated is false wherever no such statement is held: the provider's own model
// list (or nothing) then speaks for the model, and the adapter sends an effort
// in the provider's documented field. Where stated is true the list is the
// WHOLE truth for the route — the adapter refuses every other effort in
// Translate, and the publisher publishes exactly this list — so the catalogue
// and the request path cannot disagree. An empty list means the route takes no
// effort control at all.
//
// Today only xAI is stated, because xAI's direct API answers an effort its
// model does not take with an error rather than ignoring it, and its
// `GET /v1/models` says nothing about efforts: the catalogue's only other
// source was OpenRouter's metadata for the same line, which describes
// OpenRouter's normalized `reasoning.effort`, not xAI's `reasoning_effort`.
func ReasoningEfforts(slug contract.ProviderSlug, upstreamModelID string) (efforts []contract.ReasoningEffort, stated bool) {
	if slug != "xai" {
		return nil, false
	}
	if family := xAIFamily(upstreamModelID); family.Format != contract.APIFormatChatCompletions {
		// Speech and voice sessions have no reasoning step; their own paths
		// refuse an effort.
		return nil, false
	}
	return append([]contract.ReasoningEffort{}, xAIReasoningEfforts[upstreamModelID]...), true
}

// xAIReasoningEfforts is each xAI chat model's accepted `reasoning_effort`
// values, within the contract's vocabulary (low, medium, high), as xAI's own
// model pages state them on 2026-09-30
// (https://docs.x.ai/developers/models/<id>, "Reasoning efforts (supported)")
// and https://docs.x.ai/developers/model-capabilities/text/reasoning:
//
//	grok-4.7, grok-4.6, grok-4.5   low, medium, high, xhigh
//	grok-4.3                       none, low, medium, high, xhigh
//	grok-4.20-0309-reasoning       reasoning: yes, no effort listed
//	grok-4.20-0309-non-reasoning   reasoning: no
//	grok-build-0.1                 reasoning: yes, no effort listed
//	grok-4.20-multi-agent-0309     the effort selects the AGENT COUNT (4 or 16),
//	                               not reasoning depth
//
// `none` and `xhigh` are outside the contract and so cannot be asked for. A
// model xAI documents with no effort takes NONE: sending one is exactly the
// upstream error this table exists to prevent, and a model whose effort means
// something else (multi-agent) would report a control that did not do what the
// caller asked. An id absent from the table was never reviewed and is treated
// the same way. OpenRouter's metadata agrees on the same day: it lists
// `supported_efforts` for grok-4.7/4.6/4.5/4.3 and none for grok-build-0.1 or
// grok-4.20.
//
// Every xAI chat id attributed in configs/model-attribution.json has a row;
// `TestEveryAttributedXAIChatModelHasAReviewedEffortStatement` pins that with
// an exact count, so a newly attributed id is reviewed here rather than
// defaulting to "none" unnoticed.
var xAIReasoningEfforts = map[string][]contract.ReasoningEffort{
	"grok-4.7": {contract.ReasoningEffortLow, contract.ReasoningEffortMedium, contract.ReasoningEffortHigh},
	"grok-4.6": {contract.ReasoningEffortLow, contract.ReasoningEffortMedium, contract.ReasoningEffortHigh},
	"grok-4.5": {contract.ReasoningEffortLow, contract.ReasoningEffortMedium, contract.ReasoningEffortHigh},
	"grok-4.3": {contract.ReasoningEffortLow, contract.ReasoningEffortMedium, contract.ReasoningEffortHigh},

	"grok-4.20-0309-reasoning":     {},
	"grok-4.20-0309-non-reasoning": {},
	"grok-4.20-multi-agent-0309":   {},
	"grok-build-0.1":               {},
}
