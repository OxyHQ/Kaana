package contract

import (
	"fmt"
	"math"
	"slices"
)

// RealtimeParameters is what one provider's realtime WIRE takes, stated by
// providerconfig.RealtimeStatement from a reviewed reading of its
// documentation. It is Kaana's own knowledge and never reaches a client
// (Kaana ADR 0003): a client speaks the contract alone, and Kaana translates
// or emulates each contract value onto this, refusing only what no
// translation can serve (Unservable). The inventory carries it so the public
// API can refuse that before a grant; the catalogue does not publish it.
type RealtimeParameters struct {
	// TurnDetection is every turn detection the wire names, each once, in the
	// contract's order.
	TurnDetection []RealtimeTurnDetectionParameters `json:"turnDetection"`
	// OutputModalities is every set of output modalities one response of the
	// wire may be asked for, each set sorted, the sets sorted.
	OutputModalities [][]RealtimeOutputModality `json:"outputModalities"`
	// ToolChoice is every tool choice the wire names: the modes, and
	// `function` for a choice that names one tool. `auto` is always there:
	// it is what a session with tools does when it is not told otherwise.
	ToolChoice []RealtimeToolChoice `json:"toolChoice"`
	// MaxOutputTokens is the largest per-response output cap the wire takes;
	// absent, it has none.
	MaxOutputTokens *int `json:"maxOutputTokens,omitempty"`
	// InputTranscriptionPrompt is whether the wire's input transcription takes
	// a prompt. Without one a prompt, which the contract defines as a hint, is
	// not sent.
	InputTranscriptionPrompt bool `json:"inputTranscriptionPrompt"`
}

// RealtimeTurnDetectionParameters is one turn detection the wire names.
type RealtimeTurnDetectionParameters struct {
	Type RealtimeTurnDetectionType `json:"type"`
	// FixedResponses is a detection that always answers the turn it detected
	// and lets the caller's speech interrupt the answer: it has neither
	// switch, so a session that turns either off cannot be served.
	FixedResponses bool `json:"fixedResponses"`
	// Threshold is the wire's own range for server_vad's `threshold`, onto
	// which the contract's 0 to 1 is mapped; absent, the wire's range is the
	// contract's.
	Threshold *NumberRange `json:"threshold,omitempty"`
	// MeteredUnits are the units a session under this detection is billed in,
	// sorted.
	MeteredUnits []UsageUnit `json:"meteredUnits"`
}

// NumberRange is a closed interval.
type NumberRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Map is the point of this range at the same position as value in the
// contract's own range 0 to 1: the contract's lowest threshold is the wire's
// lowest, its highest the wire's highest. Rounded to a thousandth, finer than
// any provider documents a threshold, so the wire carries 0.66 rather than
// 0.6599999999999999.
func (r NumberRange) Map(value float64) float64 {
	return math.Round((r.Min+value*(r.Max-r.Min))*1000) / 1000
}

// RealtimeToolChoice is one kind of tool choice.
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

// TurnDetectionOf is the wire's parameters of one turn detection type.
func (p RealtimeParameters) TurnDetectionOf(kind RealtimeTurnDetectionType) (RealtimeTurnDetectionParameters, bool) {
	index := slices.IndexFunc(p.TurnDetection, func(t RealtimeTurnDetectionParameters) bool { return t.Type == kind })
	if index < 0 {
		return RealtimeTurnDetectionParameters{}, false
	}
	return p.TurnDetection[index], true
}

// WireTurnDetection is the detection the wire runs for the contract's kind:
// the same kind, or server_vad for a semantic_vad the wire does not name
// (both end a turn on the caller's pause; semantic_vad is the finer reading
// of it). ok is false when the wire has neither.
func (p RealtimeParameters) WireTurnDetection(kind RealtimeTurnDetectionType) (RealtimeTurnDetectionParameters, bool) {
	if served, ok := p.TurnDetectionOf(kind); ok {
		return served, true
	}
	if kind == TurnDetectionSemanticVAD {
		return p.TurnDetectionOf(TurnDetectionServerVAD)
	}
	return RealtimeTurnDetectionParameters{}, false
}

// ServesOutputModalities reports whether one response of the wire may be
// asked for exactly these modalities, in any order.
func (p RealtimeParameters) ServesOutputModalities(modalities []RealtimeOutputModality) bool {
	wanted := slices.Sorted(slices.Values(modalities))
	return slices.ContainsFunc(p.OutputModalities, func(set []RealtimeOutputModality) bool { return slices.Equal(set, wanted) })
}

// WireOutputModalities is what one response asks the wire for, to give the
// client the contract modalities it asked for: those, when the wire takes
// them; otherwise audio, whose transcript is the response's text and whose
// frames are not relayed to a client that asked for none (the session filters
// them).
func (p RealtimeParameters) WireOutputModalities(asked []RealtimeOutputModality) []RealtimeOutputModality {
	if asked == nil || p.ServesOutputModalities(asked) {
		return asked
	}
	return []RealtimeOutputModality{RealtimeOutputAudio}
}

// RealtimeParameterIssue is one contract field a configuration sets that no
// translation onto the wire can serve: the param it names and why, in the
// contract's terms, never the provider's.
type RealtimeParameterIssue struct {
	Param   string
	Message string
}

// Unservable is every field of a session configuration, an update or a
// response.create that cannot be served on this wire even by translation or
// emulation (Kaana ADR 0003), in the contract's field order. prefix is
// `config` or `response`.
func (p RealtimeParameters) Unservable(prefix string, fields RealtimeConfigFields) []RealtimeParameterIssue {
	var issues []RealtimeParameterIssue
	issue := func(param, format string, args ...any) {
		issues = append(issues, RealtimeParameterIssue{Param: prefix + "." + param, Message: fmt.Sprintf(format, args...)})
	}
	if detection := fields.TurnDetection; detection != nil {
		served, ok := p.WireTurnDetection(detection.Type)
		switch {
		case !ok:
			issue("turnDetection", "this model cannot detect turns by %s", detection.Type)
		case served.FixedResponses && (detection.CreateResponse == nil || !*detection.CreateResponse):
			issue("turnDetection.createResponse", "this model always answers a turn it detected")
		case served.FixedResponses && (detection.InterruptResponse == nil || !*detection.InterruptResponse):
			issue("turnDetection.interruptResponse", "this model always lets the caller interrupt an answer")
		}
	}
	if fields.ToolChoice != nil {
		kind := RealtimeToolChoiceOf(*fields.ToolChoice)
		// `none` for the session is served by withholding the tools; one
		// response cannot withhold them where the wire has no choice.
		withheld := kind == RealtimeToolChoiceNone && prefix == "config"
		if !slices.Contains(p.ToolChoice, kind) && !withheld {
			issue("toolChoice", "this model cannot be made to %s", toolChoiceVerb(kind))
		}
	}
	if fields.MaxOutputTokens != nil && p.MaxOutputTokens == nil {
		issue("maxOutputTokens", "this model cannot cap the output tokens of a response")
	}
	return issues
}

func toolChoiceVerb(kind RealtimeToolChoice) string {
	switch kind {
	case RealtimeToolChoiceNone:
		return "answer one response without its tools"
	case RealtimeToolChoiceRequired:
		return "call a tool on every response"
	default:
		return "call one named tool"
	}
}

// RealtimeConfigFields is the part of a configuration, an update or a
// response.create that Unservable reads.
type RealtimeConfigFields struct {
	TurnDetection   *RealtimeTurnDetection
	ToolChoice      *ToolChoice
	MaxOutputTokens *int
}

// Fields is a session configuration's.
func (c RealtimeSessionConfig) Fields() RealtimeConfigFields {
	detection := c.TurnDetection
	return RealtimeConfigFields{TurnDetection: &detection, ToolChoice: c.ToolChoice, MaxOutputTokens: c.MaxOutputTokens}
}

// Fields is an update's.
func (u RealtimeSessionConfigUpdate) Fields() RealtimeConfigFields {
	return RealtimeConfigFields{TurnDetection: u.TurnDetection, ToolChoice: u.ToolChoice, MaxOutputTokens: u.MaxOutputTokens}
}

// Fields is a response.create's.
func (r RealtimeResponseParameters) Fields() RealtimeConfigFields {
	return RealtimeConfigFields{ToolChoice: r.ToolChoice, MaxOutputTokens: r.MaxOutputTokens}
}

// NeverResponds is a transcriber's statement: no response can be asked for,
// so it names no output modality set, no tool choice and no output cap.
func (p RealtimeParameters) NeverResponds() bool {
	return len(p.OutputModalities) == 0 && len(p.ToolChoice) == 0 && p.MaxOutputTokens == nil
}

// Validate refuses a statement Kaana could not translate onto: each value
// once and in order, `auto` named, a range that is a range. A statement that
// never responds (a transcriber's) names neither modalities nor tool choices.
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
	if p.NeverResponds() {
		return nil
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
		return fmt.Errorf("toolChoice names auto")
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
