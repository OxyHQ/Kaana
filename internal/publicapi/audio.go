package publicapi

import (
	"fmt"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// AudioCapabilities is what a speech or transcription model publishes about
// its voice limits (ADR 0002, decision 9; modelAudioCapabilitiesSchema). When
// several providers serve a line it is the intersection of what each accepts.
type AudioCapabilities struct {
	Voices             []string
	Languages          []string
	InputMediaTypes    []contract.AudioInputMediaType
	OutputMediaTypes   []contract.AudioMediaType
	Speed              *SpeedRange
	MaxInputCharacters *int
}

// SpeedRange is the speed a model accepts, inclusive.
type SpeedRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// AudioCatalogue answers a model's published audio capabilities. A model the
// catalogue does not describe is checked only against the contract's bounds
// before the grant, and its adapter refuses what it cannot express after.
type AudioCatalogue interface {
	Audio(model contract.ModelReference) (*AudioCapabilities, bool)
}

// speechIssue is checkSpeechRequest's issue: the field at fault and why.
type speechIssue struct {
	param   string
	message string
}

// checkSpeech is `checkSpeechRequest` of `@kaana.ai/contracts`, restated: the
// first issue refuses the request with invalid_request BEFORE a grant is asked
// for, so a speed a model refuses holds no money.
func checkSpeech(request contract.SpeechRequest, audio *AudioCapabilities) []speechIssue {
	var issues []speechIssue
	if audio.MaxInputCharacters != nil {
		if length := utf16Length(request.Input); length > *audio.MaxInputCharacters {
			issues = append(issues, speechIssue{"input", fmt.Sprintf("input is %d characters; this model reads at most %d", length, *audio.MaxInputCharacters)})
		}
	}
	if !slices.Contains(audio.Voices, request.Voice) {
		issues = append(issues, speechIssue{"voice", "this model has no voice " + request.Voice})
	}
	format := speechFormatOf(request)
	if !slices.Contains(audio.OutputMediaTypes, speechMediaType(format)) {
		issues = append(issues, speechIssue{"response_format", fmt.Sprintf("this model does not produce %s", format)})
	}
	if request.Speed != nil {
		switch {
		case audio.Speed == nil:
			issues = append(issues, speechIssue{"speed", "this model accepts no speed control"})
		case *request.Speed < audio.Speed.Min || *request.Speed > audio.Speed.Max:
			issues = append(issues, speechIssue{"speed", fmt.Sprintf("speed must be between %g and %g for this model", audio.Speed.Min, audio.Speed.Max)})
		}
	}
	return issues
}

func speechFormatOf(request contract.SpeechRequest) contract.SpeechFormat {
	if request.ResponseFormat == nil {
		return "mp3"
	}
	return *request.ResponseFormat
}

// speechMediaType is `speechMediaType` of the contract.
func speechMediaType(format contract.SpeechFormat) contract.AudioMediaType {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	}
	return "audio/pcm"
}
