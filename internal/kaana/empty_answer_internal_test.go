package kaana

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// TestOnlyATextAnswerCanBeEmpty: an embedding, a decisions answer, speech,
// transcription or an image is not a stream of text, so having no deltas says
// nothing about it. The control is the same empty completion on a text
// endpoint, which is a failure.
func TestOnlyATextAnswerCanBeEmpty(t *testing.T) {
	outcome := provider.Outcome{FinishReason: contract.FinishStop, Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}}
	for format, want := range map[contract.APIFormat]bool{
		contract.APIFormatResponses:           true,
		contract.APIFormatChatCompletions:     true,
		contract.APIFormatEmbeddings:          false,
		contract.APIFormatDecisions:           false,
		contract.APIFormatAudioSpeech:         false,
		contract.APIFormatAudioTranscriptions: false,
		contract.APIFormatImagesGenerations:   false,
		contract.APIFormatRerank:              false,
		contract.APIFormatBatches:             false,
	} {
		emit := newEmitter(func(contract.StreamEvent) error { return nil }, &contract.Request{}, nil, time.Now())
		emit.attemptStarted = true
		if got := producedNoOutput(format, nil, emit, outcome); got != want {
			t.Errorf("%s: producedNoOutput = %t, want %t", format, got, want)
		}
	}
}
