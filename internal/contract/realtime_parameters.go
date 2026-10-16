package contract

import (
	"fmt"
	"slices"
)

// RealtimeParameters is what a realtime model's session configuration may set,
// and within which bounds: `capabilities.realtime.parameters` (contract set
// 5.0.0). It is the reviewed statement of the protocol that serves the model
// (providerconfig.RealtimeStatement), and three readers hold to it: the
// adapter's dialect refuses exactly what it omits, checkRealtimeOpen refuses
// it before a grant is asked for, and a client builds its session.open from
// it. A field the contract has and this does not name is refused, never
// dropped.
type RealtimeParameters struct {
	// TurnDetection is every turn detection the model serves, each once, in
	// the contract's order.
	TurnDetection []RealtimeTurnDetectionParameters `json:"turnDetection"`
	// OutputModalities is every set of output modalities one response may
	// produce, each set sorted, the sets sorted.
	OutputModalities [][]RealtimeOutputModality `json:"outputModalities"`
	// ToolChoice is every tool choice served: the modes, and `function` for a
	// choice that names one tool. `auto` is always served: it is what a
	// session with tools does when it is not told otherwise.
	ToolChoice []RealtimeToolChoice `json:"toolChoice"`
	// MaxOutputTokens is the most output tokens one response may be capped
	// at; absent, the model has no such cap and `maxOutputTokens` is refused.
	MaxOutputTokens *int `json:"maxOutputTokens,omitempty"`
	// InputTranscriptionPrompt is whether input transcription takes a prompt.
	InputTranscriptionPrompt bool `json:"inputTranscriptionPrompt"`
}

// RealtimeTurnDetectionParameters is one turn detection a model serves.
type RealtimeTurnDetectionParameters struct {
	Type RealtimeTurnDetectionType `json:"type"`
	// FixedResponses is a model whose detection always answers the turn it
	// detected and lets the caller's speech interrupt the answer: it has
	// neither switch, so `createResponse` and `interruptResponse` must both be
	// true.
	FixedResponses bool `json:"fixedResponses"`
	// Threshold bounds server_vad's `threshold`; absent, the contract's 0 to 1.
	Threshold *NumberRange `json:"threshold,omitempty"`
	// MeteredUnits are the units a session under this detection is billed in,
	// sorted: what a client multiplies the model's prices by to estimate a
	// call. A provider that bills a session by its clock under one detection
	// and by its audio under another states both.
	MeteredUnits []UsageUnit `json:"meteredUnits"`
}

// NumberRange is a closed interval.
type NumberRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Contains reports whether value is inside the range.
func (r NumberRange) Contains(value float64) bool { return value >= r.Min && value <= r.Max }

// RealtimeToolChoice is one kind of tool choice a realtime model serves.
type RealtimeToolChoice string

const (
	RealtimeToolChoiceAuto     RealtimeToolChoice = "auto"
	RealtimeToolChoiceNone     RealtimeToolChoice = "none"
	RealtimeToolChoiceRequired RealtimeToolChoice = "required"
	RealtimeToolChoiceFunction RealtimeToolChoice = "function"
)

var realtimeToolChoiceValues = []RealtimeToolChoice{
	RealtimeToolChoiceAuto, RealtimeToolChoiceNone, RealtimeToolChoiceRequired, RealtimeToolChoiceFunction,
}

// Valid reports whether the kind is one the contract declares.
func (c RealtimeToolChoice) Valid() bool { return isMember(c, realtimeToolChoiceValues) }

// RealtimeToolChoiceOf is the kind of a contract tool choice.
func RealtimeToolChoiceOf(choice ToolChoice) RealtimeToolChoice {
	if choice.Function != nil {
		return RealtimeToolChoiceFunction
	}
	return RealtimeToolChoice(*choice.Mode)
}

// TurnDetectionOf is the parameters of one turn detection type, if served.
func (p RealtimeParameters) TurnDetectionOf(kind RealtimeTurnDetectionType) (RealtimeTurnDetectionParameters, bool) {
	index := slices.IndexFunc(p.TurnDetection, func(t RealtimeTurnDetectionParameters) bool { return t.Type == kind })
	if index < 0 {
		return RealtimeTurnDetectionParameters{}, false
	}
	return p.TurnDetection[index], true
}

// ServesOutputModalities reports whether one response may produce exactly
// these modalities, in any order.
func (p RealtimeParameters) ServesOutputModalities(modalities []RealtimeOutputModality) bool {
	wanted := slices.Sorted(slices.Values(modalities))
	return slices.ContainsFunc(p.OutputModalities, func(set []RealtimeOutputModality) bool { return slices.Equal(set, wanted) })
}

// RealtimeParameterIssue is one field a session configuration sets that the
// model's parameters do not serve: the param it names and why.
type RealtimeParameterIssue struct {
	Param   string
	Message string
}

// Issues is every field of a session configuration, or of an update to one,
// that these parameters do not serve, in the order the contract lists them.
// prefix is `config` for a session.open and a session.update.
func (p RealtimeParameters) Issues(prefix string, fields RealtimeConfigFields) []RealtimeParameterIssue {
	var issues []RealtimeParameterIssue
	issue := func(param, format string, args ...any) {
		issues = append(issues, RealtimeParameterIssue{Param: prefix + "." + param, Message: fmt.Sprintf(format, args...)})
	}
	if fields.OutputModalities != nil && !p.ServesOutputModalities(fields.OutputModalities) {
		issue("outputModalities", "this model produces %s in one response", describeModalitySets(p.OutputModalities))
	}
	if detection := fields.TurnDetection; detection != nil {
		served, ok := p.TurnDetectionOf(detection.Type)
		switch {
		case !ok:
			issue("turnDetection", "this model has no %s turn detection", detection.Type)
		default:
			if served.FixedResponses && (detection.CreateResponse == nil || !*detection.CreateResponse) {
				issue("turnDetection.createResponse", "this model's %s always answers the turn it detected; createResponse must be true", detection.Type)
			}
			if served.FixedResponses && (detection.InterruptResponse == nil || !*detection.InterruptResponse) {
				issue("turnDetection.interruptResponse", "this model's %s lets the caller's speech interrupt an answer; interruptResponse must be true", detection.Type)
			}
			if served.Threshold != nil && detection.Threshold != nil && !served.Threshold.Contains(*detection.Threshold) {
				issue("turnDetection.threshold", "this model's %s threshold is %g to %g", detection.Type, served.Threshold.Min, served.Threshold.Max)
			}
		}
	}
	if fields.InputTranscriptionPrompt && !p.InputTranscriptionPrompt {
		issue("inputAudioTranscription.prompt", "this model's input transcription takes no prompt")
	}
	if fields.ToolChoice != nil && !slices.Contains(p.ToolChoice, RealtimeToolChoiceOf(*fields.ToolChoice)) {
		issue("toolChoice", "this model serves the tool choices %v", p.ToolChoice)
	}
	if fields.MaxOutputTokens != nil {
		switch {
		case p.MaxOutputTokens == nil:
			issue("maxOutputTokens", "this model has no output token cap")
		case *fields.MaxOutputTokens > *p.MaxOutputTokens:
			issue("maxOutputTokens", "this model caps a response at most at %d output tokens", *p.MaxOutputTokens)
		}
	}
	return issues
}

// RealtimeConfigFields is the part of a session configuration, or of an
// update to one, that RealtimeParameters governs.
type RealtimeConfigFields struct {
	OutputModalities         []RealtimeOutputModality
	TurnDetection            *RealtimeTurnDetection
	InputTranscriptionPrompt bool
	ToolChoice               *ToolChoice
	MaxOutputTokens          *int
}

// Fields is a session configuration's governed fields.
func (c RealtimeSessionConfig) Fields() RealtimeConfigFields {
	detection := c.TurnDetection
	return RealtimeConfigFields{
		OutputModalities: c.OutputModalities, TurnDetection: &detection,
		InputTranscriptionPrompt: c.InputAudioTranscription != nil && c.InputAudioTranscription.Prompt != nil,
		ToolChoice:               c.ToolChoice, MaxOutputTokens: c.MaxOutputTokens,
	}
}

// Fields is an update's governed fields.
func (u RealtimeSessionConfigUpdate) Fields() RealtimeConfigFields {
	return RealtimeConfigFields{
		OutputModalities: u.OutputModalities, TurnDetection: u.TurnDetection,
		InputTranscriptionPrompt: u.InputAudioTranscription != nil && u.InputAudioTranscription.Prompt != nil,
		ToolChoice:               u.ToolChoice, MaxOutputTokens: u.MaxOutputTokens,
	}
}

// Fields is a response.create's governed fields.
func (r RealtimeResponseParameters) Fields() RealtimeConfigFields {
	return RealtimeConfigFields{OutputModalities: r.OutputModalities, ToolChoice: r.ToolChoice, MaxOutputTokens: r.MaxOutputTokens}
}

func describeModalitySets(sets [][]RealtimeOutputModality) string {
	described := ""
	for index, set := range sets {
		if index > 0 {
			described += " or "
		}
		described += fmt.Sprint(set)
	}
	return described
}

// Validate refuses parameters the catalogue could not publish: each value
// once and in order, `auto` served, a range that is a range.
func (p RealtimeParameters) Validate() error {
	if len(p.TurnDetection) == 0 {
		return fmt.Errorf("turnDetection names at least one detection")
	}
	for index, detection := range p.TurnDetection {
		if !isMember(detection.Type, realtimeTurnDetectionTypeValues) {
			return fmt.Errorf("turnDetection[%d].type %q is not a turn detection", index, detection.Type)
		}
		if index > 0 && slices.Index(realtimeTurnDetectionTypeValues, p.TurnDetection[index-1].Type) >= slices.Index(realtimeTurnDetectionTypeValues, detection.Type) {
			return fmt.Errorf("turnDetection is in the contract's order and names each type once")
		}
		if detection.Threshold != nil && (detection.Threshold.Min < 0 || detection.Threshold.Max > 1 || detection.Threshold.Min > detection.Threshold.Max) {
			return fmt.Errorf("turnDetection[%d].threshold is a range inside 0 to 1", index)
		}
		if len(detection.MeteredUnits) == 0 || !slices.IsSorted(detection.MeteredUnits) || len(slices.Compact(slices.Clone(detection.MeteredUnits))) != len(detection.MeteredUnits) {
			return fmt.Errorf("turnDetection[%d].meteredUnits names at least one unit, sorted, each once", index)
		}
		for _, unit := range detection.MeteredUnits {
			if !unit.Valid() {
				return fmt.Errorf("turnDetection[%d].meteredUnits names %q, which is not a unit", index, unit)
			}
		}
	}
	if len(p.OutputModalities) == 0 {
		return fmt.Errorf("outputModalities names at least one set")
	}
	for index, set := range p.OutputModalities {
		if len(set) == 0 || !slices.IsSorted(set) || len(slices.Compact(slices.Clone(set))) != len(set) {
			return fmt.Errorf("outputModalities[%d] is a sorted set of at least one modality", index)
		}
		for _, modality := range set {
			if !isMember(modality, realtimeOutputModalityValues) {
				return fmt.Errorf("outputModalities[%d] names %q, which is not a modality", index, modality)
			}
		}
		if index > 0 && slices.Compare(p.OutputModalities[index-1], set) >= 0 {
			return fmt.Errorf("outputModalities is sorted and names each set once")
		}
	}
	if !slices.Contains(p.ToolChoice, RealtimeToolChoiceAuto) {
		return fmt.Errorf("toolChoice serves auto")
	}
	for index, choice := range p.ToolChoice {
		if !choice.Valid() {
			return fmt.Errorf("toolChoice[%d] %q is not a tool choice", index, choice)
		}
		if index > 0 && slices.Index(realtimeToolChoiceValues, p.ToolChoice[index-1]) >= slices.Index(realtimeToolChoiceValues, choice) {
			return fmt.Errorf("toolChoice is in the contract's order and names each kind once")
		}
	}
	if p.MaxOutputTokens != nil && *p.MaxOutputTokens <= 0 {
		return fmt.Errorf("maxOutputTokens is positive")
	}
	return nil
}
