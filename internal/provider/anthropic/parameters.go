package anthropic

import (
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// messagesControls is every request control this adapter can send through the
// Messages API, in the inventory's vocabulary. Translate refuses the rest
// (penalties, `seed`, a non-text response format) before sending anything.
var messagesControls = []provider.RequestParameter{
	provider.ParameterMaxOutputTokens,
	provider.ParameterReasoningEffort,
	provider.ParameterStopSequences,
	provider.ParameterTemperature,
	provider.ParameterTopP,
	provider.ParameterToolChoice,
	provider.ParameterToolChoiceFunction,
	provider.ParameterToolChoiceNone,
	provider.ParameterToolChoiceRequired,
	provider.ParameterTools,
}

// fixedSampling: "Setting temperature, top_p, or top_k to any non-default
// value on Claude Opus 4.7 and later models ... returns a 400 error"
// (platform.claude.com/docs/en/models/opus-5-5/migration-guide, read
// 2026-10-12; the Sonnet 5, Sonnet 5.5, Haiku 5.5, Fable 5 and Fable 5.1 pages
// say the same of those models). The accepted-parameter vocabulary is
// presence-only, so the control is not accepted at all.
var fixedSampling = []provider.RequestParameter{provider.ParameterTemperature, provider.ParameterTopP}

// noForcedToolChoice: "`tool_choice` types `any` and `tool` return a 400 error
// (`tool_choice: type "tool" and "any" are not supported for this model.`)"
// on Claude Fable 5.1, Opus 5.5 and Sonnet 5.5, which take `auto` (the
// default) and `none` (their migration guides, read 2026-10-12).
var noForcedToolChoice = []provider.RequestParameter{provider.ParameterToolChoiceRequired, provider.ParameterToolChoiceFunction}

// refusedByModel is, for every Anthropic model id attributed in
// configs/model-attribution.json, the controls Anthropic documents that model
// as rejecting. None of it is in the Models API's capability tree, which is
// why it is reviewed here; everything the tree DOES say (effort, modalities,
// limits) is read from it by the publisher. A model id absent from this table
// was never reviewed: AcceptedParameters states nothing for it, and the
// catalogue says nothing. `TestEveryAttributedAnthropicModelHasAReviewedParameterStatement`
// pins the table to the attribution with an exact count.
var refusedByModel = map[string][]provider.RequestParameter{
	"claude-fable-5-1":          concat(fixedSampling, noForcedToolChoice),
	"claude-fable-5":            fixedSampling,
	"claude-opus-5-5":           concat(fixedSampling, noForcedToolChoice),
	"claude-opus-5":             fixedSampling,
	"claude-opus-4-8":           fixedSampling,
	"claude-opus-4-7":           fixedSampling,
	"claude-opus-4-6":           nil,
	"claude-opus-4-5-20251101":  nil,
	"claude-sonnet-5-5":         concat(fixedSampling, noForcedToolChoice),
	"claude-sonnet-5":           fixedSampling,
	"claude-sonnet-4-6":         nil,
	"claude-haiku-5-5":          fixedSampling,
	"claude-haiku-4-5-20251001": nil,
}

func concat(lists ...[]provider.RequestParameter) []provider.RequestParameter {
	var all []provider.RequestParameter
	for _, list := range lists {
		all = append(all, list...)
	}
	return all
}

// AcceptedParameters is the controls one Claude model takes through this
// adapter: every control the Messages API carries, less what Anthropic
// documents the model as rejecting. stated is false for a model id nobody
// reviewed.
//
// It is the adapter's own statement, as providerconfig.ReasoningEfforts is
// for xAI: Translate refuses exactly what it leaves out, and the publisher
// publishes it as the deployment's acceptedParameters, so the catalogue and
// the request path cannot disagree. Whether the model takes an effort at all
// is Anthropic's published `capabilities.effort`, which the publisher
// intersects in.
func AcceptedParameters(upstreamModelID string) (parameters []provider.RequestParameter, stated bool) {
	refused, stated := refusedByModel[upstreamModelID]
	if !stated {
		return nil, false
	}
	refusedSet := make(map[provider.RequestParameter]bool, len(refused))
	for _, parameter := range refused {
		refusedSet[parameter] = true
	}
	parameters = make([]provider.RequestParameter, 0, len(messagesControls))
	for _, parameter := range messagesControls {
		if !refusedSet[parameter] {
			parameters = append(parameters, parameter)
		}
	}
	return parameters, true
}

// carriedParameters is what a request makes this adapter send, in the
// vocabulary, for the controls the Messages API carries. It reads the
// contract request, as Translate encodes it: `maxOutputTokens` is mandatory
// here, an empty stop list sends nothing, and `auto` needs no tool-choice
// value (provider.ToolChoiceParameters).
func carriedParameters(request *contract.Request) []provider.RequestParameter {
	carried := []provider.RequestParameter{provider.ParameterMaxOutputTokens}
	if request.Reasoning != nil {
		carried = append(carried, provider.ParameterReasoningEffort)
	}
	if len(request.Sampling.StopSequences) > 0 {
		carried = append(carried, provider.ParameterStopSequences)
	}
	if request.Sampling.Temperature != nil {
		carried = append(carried, provider.ParameterTemperature)
	}
	if request.Sampling.TopP != nil {
		carried = append(carried, provider.ParameterTopP)
	}
	if len(request.Tools) > 0 {
		carried = append(carried, provider.ParameterTools)
	}
	return append(carried, provider.ToolChoiceParameters(request.ToolChoice)...)
}

// refuseUnacceptedParameters refuses, before anything is sent, a control the
// model is documented to reject (AcceptedParameters) or the route's published
// set lacks. Anthropic answers either with an `invalid_request_error` that
// Kaana treats as terminal, so sending it would only spend the request.
func refuseUnacceptedParameters(request *contract.Request, route provider.Route) error {
	carried := carriedParameters(request)
	if accepted, stated := AcceptedParameters(route.UpstreamModelID); stated {
		reviewed := provider.Route{AcceptedParameters: &accepted}
		if parameter, refused := reviewed.UnacceptedParameter(carried...); refused {
			return provider.ErrUnsupported{
				Code:   contract.CodeInvalidRequest,
				Param:  parameter.Field(),
				Detail: fmt.Sprintf("anthropic documents that %s rejects %s; omit it to use the model's own behaviour", route.UpstreamModelID, describe(parameter)),
			}
		}
	}
	if parameter, refused := route.UnacceptedParameter(carried...); refused {
		return provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  parameter.Field(),
			Detail: fmt.Sprintf("anthropic reports that this model does not accept %s; omit it to use the model's own behaviour", describe(parameter)),
		}
	}
	return nil
}

func describe(parameter provider.RequestParameter) string {
	switch parameter {
	case provider.ParameterToolChoiceFunction:
		return "a tool choice that forces one named tool"
	case provider.ParameterToolChoiceRequired:
		return "a tool choice that forces a tool call (required)"
	case provider.ParameterToolChoiceNone:
		return "the tool choice none"
	}
	return string(parameter)
}
