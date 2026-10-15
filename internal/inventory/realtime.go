package inventory

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// RealtimeObservation is what a deployment serves as a realtime session: the
// inventory's statement of the contract's `capabilities.realtime` (contract
// set 4.1.0). Neither realtime provider's model list says anything about
// sessions, so the publisher writes the adapter's own reviewed statement here
// (providerconfig.RealtimeCapabilities), exactly as it does reasoningEfforts.
// Every list is sorted and holds each value once.
type RealtimeObservation struct {
	Transports   []contract.RealtimeSessionTransport `json:"transports"`
	SessionKinds []contract.RealtimeSessionKind      `json:"sessionKinds"`
	// InputAudioTranscription is whether a conversation transcribes the
	// caller's speech when asked.
	InputAudioTranscription bool                 `json:"inputAudioTranscription"`
	AudioFormats            RealtimeAudioFormats `json:"audioFormats"`
}

// RealtimeAudioFormats is the encodings a session may name, per direction.
type RealtimeAudioFormats struct {
	Input  []contract.RealtimeAudioFormat `json:"input"`
	Output []contract.RealtimeAudioFormat `json:"output"`
}

// AudioObservation is a model's voices and languages: the inventory's
// statement of the contract's `audio` for a realtime model. `languages` empty
// means the provider documents none; `gender` empty is unknown.
type AudioObservation struct {
	Voices    []VoiceObservation `json:"voices"`
	Languages []string           `json:"languages"`
}

// VoiceObservation is one voice.
type VoiceObservation struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Languages []string `json:"languages"`
	Gender    string   `json:"gender,omitempty"`
}

// languageTag is the contract's BCP 47 grammar (`languageTagSchema`, at most
// 35 characters).
var languageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)

// maxVoiceIDLength and maxVoiceNameLength are the contract's voiceSchema
// bounds.
const (
	maxVoiceIDLength   = 64
	maxVoiceNameLength = 200
)

// Validate refuses a realtime statement the catalogue could not publish.
func (r RealtimeObservation) Validate() error {
	if err := sortedSet("transports", r.Transports, 1, func(t contract.RealtimeSessionTransport) bool { return t.Valid() }); err != nil {
		return err
	}
	if err := sortedSet("sessionKinds", r.SessionKinds, 1, func(k contract.RealtimeSessionKind) bool { return k.Valid() }); err != nil {
		return err
	}
	valid := func(f contract.RealtimeAudioFormat) bool { return f.Valid() }
	if err := sortedSet("audioFormats.input", r.AudioFormats.Input, 1, valid); err != nil {
		return err
	}
	return sortedSet("audioFormats.output", r.AudioFormats.Output, 0, valid)
}

// Validate refuses voices and languages the catalogue could not publish:
// what the contract's modelAudioCapabilitiesSchema refuses, plus order.
func (a AudioObservation) Validate() error {
	isTag := func(tag string) bool { return len(tag) <= 35 && languageTag.MatchString(tag) }
	if err := sortedSet("languages", a.Languages, 0, isTag); err != nil {
		return err
	}
	if !sort.SliceIsSorted(a.Voices, func(i, j int) bool { return a.Voices[i].ID < a.Voices[j].ID }) {
		return fmt.Errorf("voices must be sorted by id")
	}
	for index, voice := range a.Voices {
		if !boundedName(voice.ID, maxVoiceIDLength) || !boundedName(voice.Name, maxVoiceNameLength) {
			return fmt.Errorf("voices[%d] needs a trimmed id of at most %d and name of at most %d characters", index, maxVoiceIDLength, maxVoiceNameLength)
		}
		if index > 0 && a.Voices[index-1].ID == voice.ID {
			return fmt.Errorf("voices names %q twice", voice.ID)
		}
		switch voice.Gender {
		case "", "female", "male", "neutral":
		default:
			return fmt.Errorf("voices[%d].gender %q is not female, male or neutral", index, voice.Gender)
		}
		if err := sortedSet(fmt.Sprintf("voices[%d].languages", index), voice.Languages, 0, func(tag string) bool {
			return slices.Contains(a.Languages, tag)
		}); err != nil {
			return fmt.Errorf("%w (each is one of the model's languages)", err)
		}
	}
	return nil
}

func boundedName(value string, limit int) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}

// sortedSet refuses a list that is shorter than minimum, unsorted, repeats a
// value or holds one valid refuses.
func sortedSet[T ~string](name string, values []T, minimum int, valid func(T) bool) error {
	if len(values) < minimum {
		return fmt.Errorf("%s needs at least %d value(s)", name, minimum)
	}
	if values == nil {
		return fmt.Errorf("%s must be a list, empty rather than absent", name)
	}
	for index, value := range values {
		if !valid(value) {
			return fmt.Errorf("%s[%d] %q is not a value it can hold", name, index, value)
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%s must be sorted and name each value once", name)
		}
	}
	return nil
}

// aggregateRealtime narrows a line's realtime statement by one deployment's:
// the transports, kinds and encodings every reporting deployment serves, and
// transcription only if every one transcribes. A deployment that says nothing
// abstains (ADR 0002 decision 9: "the INTERSECTION — what every one of them
// accepts").
func aggregateRealtime(current, reported *RealtimeObservation) *RealtimeObservation {
	if reported == nil {
		return current
	}
	if current == nil {
		copied := *reported
		copied.Transports = slices.Clone(reported.Transports)
		copied.SessionKinds = slices.Clone(reported.SessionKinds)
		copied.AudioFormats = RealtimeAudioFormats{Input: slices.Clone(reported.AudioFormats.Input), Output: slices.Clone(reported.AudioFormats.Output)}
		return &copied
	}
	return &RealtimeObservation{
		Transports:              intersectSorted(current.Transports, reported.Transports),
		SessionKinds:            intersectSorted(current.SessionKinds, reported.SessionKinds),
		InputAudioTranscription: current.InputAudioTranscription && reported.InputAudioTranscription,
		AudioFormats: RealtimeAudioFormats{
			Input:  intersectSorted(current.AudioFormats.Input, reported.AudioFormats.Input),
			Output: intersectSorted(current.AudioFormats.Output, reported.AudioFormats.Output),
		},
	}
}

// aggregateAudio narrows a line's voices and languages by one deployment's:
// the languages every reporting deployment declares, and the voices every one
// offers, each speaking only what all of them say it speaks. A voice's name
// is the first reported (deployment-id order); its gender survives only if
// every report agrees on it.
func aggregateAudio(current, reported *AudioObservation) *AudioObservation {
	if reported == nil {
		return current
	}
	if current == nil {
		copied := AudioObservation{Languages: slices.Clone(reported.Languages), Voices: make([]VoiceObservation, 0, len(reported.Voices))}
		for _, voice := range reported.Voices {
			voice.Languages = slices.Clone(voice.Languages)
			copied.Voices = append(copied.Voices, voice)
		}
		return &copied
	}
	narrowed := AudioObservation{Languages: intersectSorted(current.Languages, reported.Languages), Voices: []VoiceObservation{}}
	for _, voice := range current.Voices {
		index := slices.IndexFunc(reported.Voices, func(other VoiceObservation) bool { return other.ID == voice.ID })
		if index < 0 {
			continue
		}
		other := reported.Voices[index]
		voice.Languages = intersectSorted(intersectSorted(voice.Languages, other.Languages), narrowed.Languages)
		if voice.Gender != other.Gender {
			voice.Gender = ""
		}
		narrowed.Voices = append(narrowed.Voices, voice)
	}
	return &narrowed
}

// intersectSorted is the values of a present in b, in a's (sorted) order;
// never nil, so an empty intersection is published as an empty list.
func intersectSorted[T comparable](a, b []T) []T {
	out := make([]T, 0, len(a))
	for _, value := range a {
		if slices.Contains(b, value) {
			out = append(out, value)
		}
	}
	return out
}
