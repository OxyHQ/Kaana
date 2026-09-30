package provider

import (
	"fmt"
	"sort"
)

// RequestParameter names one caller control a deployment may or may not
// accept, spelled as the contract request path a caller sets and a refusal
// names (`sampling.temperature`, `maxOutputTokens`, ...).
//
// It is Kaana's vocabulary, not a provider's: the publisher maps each
// provider's own words onto it (`max_tokens` and `max_completion_tokens` are
// both `maxOutputTokens`), so a catalogue consumer and Translate read one
// spelling whatever the upstream calls it.
type RequestParameter string

// The controls a deployment's accepted-parameter set can state. The list is
// closed: a spelling outside it is refused by the inventory reader, so a
// catalogue can never carry a word Translate does not check.
//
// `sampling.topK` is deliberately absent. No adapter a discovered provider is
// served through sends it (openaicompat refuses it outright), so a set that
// named it would advertise a control the route refuses.
const (
	ParameterMaxOutputTokens  RequestParameter = "maxOutputTokens"
	ParameterReasoningEffort  RequestParameter = "reasoning.effort"
	ParameterResponseFormat   RequestParameter = "responseFormat"
	ParameterFrequencyPenalty RequestParameter = "sampling.frequencyPenalty"
	ParameterPresencePenalty  RequestParameter = "sampling.presencePenalty"
	ParameterSeed             RequestParameter = "sampling.seed"
	ParameterStopSequences    RequestParameter = "sampling.stopSequences"
	ParameterTemperature      RequestParameter = "sampling.temperature"
	ParameterTopP             RequestParameter = "sampling.topP"
	ParameterToolChoice       RequestParameter = "toolChoice"
	ParameterTools            RequestParameter = "tools"
)

// RequestParameters is the whole vocabulary, sorted.
func RequestParameters() []RequestParameter {
	return []RequestParameter{
		ParameterMaxOutputTokens,
		ParameterReasoningEffort,
		ParameterResponseFormat,
		ParameterFrequencyPenalty,
		ParameterPresencePenalty,
		ParameterSeed,
		ParameterStopSequences,
		ParameterTemperature,
		ParameterTopP,
		ParameterToolChoice,
		ParameterTools,
	}
}

// Valid reports whether the parameter is in the vocabulary.
func (p RequestParameter) Valid() bool {
	for _, known := range RequestParameters() {
		if p == known {
			return true
		}
	}
	return false
}

// ValidateParameterSet refuses a set the reader could not represent
// faithfully: a word outside the vocabulary, a repeat, or an unsorted list
// (sorted so a re-issue of the same statement is byte-identical).
func ValidateParameterSet(parameters []RequestParameter) error {
	for index, parameter := range parameters {
		if !parameter.Valid() {
			return fmt.Errorf("%q at index %d is not a request parameter", parameter, index)
		}
		if index > 0 && parameters[index-1] >= parameter {
			return fmt.Errorf("must be sorted and name each parameter once (at %q)", parameter)
		}
	}
	return nil
}

// SortParameters sorts a set in place into its canonical order.
func SortParameters(parameters []RequestParameter) {
	sort.Slice(parameters, func(a, b int) bool { return parameters[a] < parameters[b] })
}

// UnacceptedParameter returns the first of `sent`, in vocabulary order, that
// the route's accepted set does not name. A route whose set is unknown (nil)
// accepts everything: absent is "nobody said", never "nothing".
func (r Route) UnacceptedParameter(sent ...RequestParameter) (RequestParameter, bool) {
	if r.AcceptedParameters == nil {
		return "", false
	}
	accepted := make(map[RequestParameter]bool, len(*r.AcceptedParameters))
	for _, parameter := range *r.AcceptedParameters {
		accepted[parameter] = true
	}
	carried := make(map[RequestParameter]bool, len(sent))
	for _, parameter := range sent {
		carried[parameter] = true
	}
	for _, parameter := range RequestParameters() {
		if carried[parameter] && !accepted[parameter] {
			return parameter, true
		}
	}
	return "", false
}
