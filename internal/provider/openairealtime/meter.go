package openairealtime

import (
	"sync"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// A provider that does not bill the tokens it reports is metered here, by
// what Kaana itself sent and received. That is xAI's Voice Agent API, whose
// pricing is "billed per minute plus a flat fee per text input message"
// (https://docs.x.ai/developers/models/speech-to-speech):
//
//	Audio $0.08 / minute
//	Text Input $0.004 per conversation.item.create event
//	"Push-to-talk sessions are billed only for audio sent and received."
//	"Every conversation.item.create event you send from the client is billed
//	at $0.004, with two exceptions: function_call_output items ... are not
//	billed. Items whose content is input_audio or audio are billed by the
//	audio meter instead."
//	"response.create is not billed as a text input."
//
// Kaana serves only push-to-talk sessions for xAI (turnDetection none; the
// server_vad session-duration charge has no contract unit, dialect.go), so the
// whole bill is three measurable quantities, each a contract unit:
//
//	audio_input_milliseconds  = decoded audio Kaana wrote upstream, at the
//	                            session's input format's byte rate
//	audio_output_milliseconds = decoded audio the provider sent, at the output
//	                            format's byte rate
//	requests                  = billed text conversation.item.create events
//
// Both audio units are priced at the same per-minute rate on the deployment's
// rate card, and `requests` at the per-event fee. The units are `oxy_measured`:
// Kaana counted them, and xAI reports no duration of its own to reconcile
// against. Milliseconds are rounded up once, over the session's total, so
// rounding never charges per event.

// bytesPerSecond is the decoded byte rate of a contract realtime audio format:
// 16-bit PCM at 24 kHz, or 8-bit G.711 at 8 kHz.
func bytesPerSecond(format contract.RealtimeAudioFormat) int64 {
	switch format {
	case contract.RealtimePCM16:
		return 48_000
	case contract.RealtimeULaw, contract.RealtimeALaw:
		return 8_000
	}
	return 0
}

// measurement is what one command will cost once it has been written.
type measurement struct {
	inputBytes int64
	textInput  bool
}

type meter struct {
	mu         sync.Mutex
	inputRate  int64
	outputRate int64
	inputBytes int64
	outBytes   int64
	textInputs int
}

func newMeter(config contract.RealtimeSessionConfig) *meter {
	m := &meter{inputRate: bytesPerSecond(config.InputAudioFormat)}
	if config.OutputAudioFormat != nil {
		m.outputRate = bytesPerSecond(*config.OutputAudioFormat)
	}
	return m
}

// measure is what a command will be billed, decided before it is written. An
// item whose billing the provider does not document is refused rather than
// guessed at: xAI bills a text item per event and an audio item by the audio
// meter, and says nothing of an item carrying both.
func (m *meter) measure(command contract.RealtimeCommand) (measurement, error) {
	switch c := command.(type) {
	case *contract.RealtimeInputAudioAppendCommand:
		return measurement{inputBytes: int64(contract.DecodedRealtimeAudioBytes(c.Data))}, nil
	case *contract.RealtimeItemCreateCommand:
		if c.Item.Type == contract.RealtimeFunctionCallOutputItem {
			return measurement{}, nil
		}
		var audio int64
		text := false
		for _, part := range c.Item.Content {
			switch part.Type {
			case contract.RealtimeInputAudioPart, contract.RealtimeOutputAudioPart:
				if part.Data != nil {
					audio += int64(contract.DecodedRealtimeAudioBytes(*part.Data))
				}
			default:
				text = true
			}
		}
		if audio > 0 && text {
			return measurement{}, refused("item.content", "xAI does not document how an item carrying both text and audio is billed; send them as separate items")
		}
		if audio > 0 {
			return measurement{inputBytes: audio}, nil
		}
		return measurement{textInput: true}, nil
	}
	return measurement{}, nil
}

// record adds a written command's measurement.
func (m *meter) record(billed measurement) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputBytes += billed.inputBytes
	if billed.textInput {
		m.textInputs++
	}
}

// output adds decoded audio the provider sent.
func (m *meter) output(decoded int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outBytes += int64(decoded)
}

func milliseconds(bytes, rate int64) int {
	return int((bytes*1000 + rate - 1) / rate)
}

// units is the session's measured totals so far.
func (m *meter) units() []contract.UsageQuantity {
	m.mu.Lock()
	defer m.mu.Unlock()
	units := []contract.UsageQuantity{}
	if m.inputBytes > 0 && m.inputRate > 0 {
		units = append(units, contract.UsageQuantity{Unit: contract.UnitAudioInputMilliseconds, Quantity: milliseconds(m.inputBytes, m.inputRate)})
	}
	if m.outBytes > 0 && m.outputRate > 0 {
		units = append(units, contract.UsageQuantity{Unit: contract.UnitAudioOutputMilliseconds, Quantity: milliseconds(m.outBytes, m.outputRate)})
	}
	if m.textInputs > 0 {
		units = append(units, contract.UsageQuantity{Unit: contract.UnitRequests, Quantity: m.textInputs})
	}
	return units
}

// meteredSession is a session whose units Kaana measured
// (provider.RealtimeMeter).
type meteredSession struct{ *session }

var _ provider.RealtimeMeter = meteredSession{}

// Measured implements provider.RealtimeMeter.
func (m meteredSession) Measured() []contract.UsageQuantity { return m.meter.units() }
