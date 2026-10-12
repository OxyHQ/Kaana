package provider

import (
	"fmt"
	"sort"

	"github.com/OxyHQ/Kaana/internal/contract"
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
//
// A tool choice is stated per VALUE, because providers accept the values
// differently: Anthropic answers a forced choice on Claude Fable 5.1, Opus 5.5
// and Sonnet 5.5 with a 400 while taking `none`, and OpenRouter publishes each
// zero-retention endpoint's `supports_tool_choice` value by value. `toolChoice`
// is the field itself; `toolChoice.none`, `toolChoice.required` and
// `toolChoice.function` (one named tool) are the values a request can carry.
// `auto` is no value at all: it is every provider's default whenever tools are
// sent, so it never needs a route to accept anything (ToolChoiceParameters).
// How a set is read when it names the field but no value is CanonicalParameterSet.
const (
	ParameterMaxOutputTokens    RequestParameter = "maxOutputTokens"
	ParameterReasoningEffort    RequestParameter = "reasoning.effort"
	ParameterResponseFormat     RequestParameter = "responseFormat"
	ParameterFrequencyPenalty   RequestParameter = "sampling.frequencyPenalty"
	ParameterPresencePenalty    RequestParameter = "sampling.presencePenalty"
	ParameterSeed               RequestParameter = "sampling.seed"
	ParameterStopSequences      RequestParameter = "sampling.stopSequences"
	ParameterTemperature        RequestParameter = "sampling.temperature"
	ParameterTopP               RequestParameter = "sampling.topP"
	ParameterToolChoice         RequestParameter = "toolChoice"
	ParameterToolChoiceFunction RequestParameter = "toolChoice.function"
	ParameterToolChoiceNone     RequestParameter = "toolChoice.none"
	ParameterToolChoiceRequired RequestParameter = "toolChoice.required"
	ParameterTools              RequestParameter = "tools"
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
		ParameterToolChoiceFunction,
		ParameterToolChoiceNone,
		ParameterToolChoiceRequired,
		ParameterTools,
	}
}

// Field is the contract request field a refusal of this control names: a
// tool-choice value is a value of `toolChoice`, not a field of its own.
func (p RequestParameter) Field() string {
	if isToolChoiceValue(p) {
		return string(ParameterToolChoice)
	}
	return string(p)
}

// ToolChoiceValues is the tool-choice values a set can state, sorted.
func ToolChoiceValues() []RequestParameter {
	return []RequestParameter{ParameterToolChoiceFunction, ParameterToolChoiceNone, ParameterToolChoiceRequired}
}

func isToolChoiceValue(parameter RequestParameter) bool {
	return parameter == ParameterToolChoiceFunction || parameter == ParameterToolChoiceNone || parameter == ParameterToolChoiceRequired
}

// ToolChoiceParameters is what a request's tool choice needs its route to
// accept: nothing for no choice or `auto` (the default whenever tools are
// sent, so an adapter may omit it), otherwise the field and the one value.
// A choice naming neither a mode nor a function needs the field; the adapter
// refuses it on its own terms.
func ToolChoiceParameters(choice *contract.ToolChoice) []RequestParameter {
	switch {
	case choice == nil:
		return nil
	case choice.Function != nil:
		return []RequestParameter{ParameterToolChoice, ParameterToolChoiceFunction}
	case choice.Mode == nil:
		return []RequestParameter{ParameterToolChoice}
	}
	switch *choice.Mode {
	case contract.ToolChoiceAuto:
		return nil
	case contract.ToolChoiceNone:
		return []RequestParameter{ParameterToolChoice, ParameterToolChoiceNone}
	case contract.ToolChoiceRequired:
		return []RequestParameter{ParameterToolChoice, ParameterToolChoiceRequired}
	default:
		return []RequestParameter{ParameterToolChoice}
	}
}

// CanonicalParameterSet is how a stated set is read, sorted:
//
//   - a set naming `toolChoice` and no tool-choice value does not distinguish
//     values — it is what a provider says when its list names only
//     `tool_choice` — so it accepts every value;
//   - a set naming a value accepts the field that carries it.
//
// Every other member is read as written. A set the publisher writes is
// already canonical; the rule is what keeps one stated before values were
// distinguished meaning what it meant.
func CanonicalParameterSet(parameters []RequestParameter) []RequestParameter {
	present := make(map[RequestParameter]bool, len(parameters)+3)
	values := false
	for _, parameter := range parameters {
		present[parameter] = true
		if isToolChoiceValue(parameter) {
			values = true
		}
	}
	if values {
		present[ParameterToolChoice] = true
	} else if present[ParameterToolChoice] {
		for _, value := range ToolChoiceValues() {
			present[value] = true
		}
	}
	canonical := make([]RequestParameter, 0, len(present))
	for _, parameter := range RequestParameters() {
		if present[parameter] {
			canonical = append(canonical, parameter)
		}
	}
	return canonical
}

// IntersectParameterSets combines two statements about one model: a control
// is accepted only when every statement that SAID something accepts it. A nil
// statement (nobody said) abstains; two nils stay unknown. Both are read
// canonically, and the field is dropped when no value survives, so the result
// can never be re-read as "every value".
func IntersectParameterSets(first, second *[]RequestParameter) *[]RequestParameter {
	if first == nil && second == nil {
		return nil
	}
	if first == nil {
		first, second = second, nil
	}
	result := CanonicalParameterSet(*first)
	if second != nil {
		other := make(map[RequestParameter]bool, len(*second))
		for _, parameter := range CanonicalParameterSet(*second) {
			other[parameter] = true
		}
		kept := result[:0]
		for _, parameter := range result {
			if other[parameter] {
				kept = append(kept, parameter)
			}
		}
		result = kept
	}
	values := false
	for _, parameter := range result {
		if isToolChoiceValue(parameter) {
			values = true
		}
	}
	if !values {
		kept := result[:0]
		for _, parameter := range result {
			if parameter != ParameterToolChoice {
				kept = append(kept, parameter)
			}
		}
		result = kept
	}
	return &result
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
// the route's accepted set does not name, read canonically
// (CanonicalParameterSet). A route whose set is unknown (nil) accepts
// everything: absent is "nobody said", never "nothing".
func (r Route) UnacceptedParameter(sent ...RequestParameter) (RequestParameter, bool) {
	if r.AcceptedParameters == nil {
		return "", false
	}
	canonical := CanonicalParameterSet(*r.AcceptedParameters)
	accepted := make(map[RequestParameter]bool, len(canonical))
	for _, parameter := range canonical {
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
