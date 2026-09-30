package openairealtime

import (
	"sync"
	"time"

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
//	"Sessions using the default server_vad turn detection are billed for
//	session duration." (https://docs.x.ai/developers/pricing)
//
// So a session is billed in exactly one of two modes, fixed when it opens
// (a session.update that would change the mode is refused, session.go):
//
// Push-to-talk (turnDetection none), three measurable quantities:
//
//	audio_input_milliseconds  = decoded audio Kaana wrote upstream, at the
//	                            session's input format's byte rate
//	audio_output_milliseconds = decoded audio the provider sent, at the output
//	                            format's byte rate
//	requests                  = billed text conversation.item.create events
//
// server_vad (the dialect's sessionClock), two:
//
//	session_milliseconds      = wall-clock time the upstream session was open,
//	                            from the accepted handshake to the upstream's
//	                            close (contract set 3.3.0)
//	requests                  = billed text conversation.item.create events
//
// and never the audio units: the clock already bills that span, and reporting
// the audio too would charge it twice. The audio units and the session clock
// are priced at the same per-minute rate on the deployment's rate card, and
// `requests` at the per-event fee. The units are `oxy_measured`: Kaana counted
// them, and xAI reports no duration of its own to reconcile against.
// Milliseconds are rounded up once, over the session's total, so rounding
// never charges per event.

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

	// clock is a session billed by its wall clock. now reads the monotonic
	// clock; openedAt is the accepted upstream handshake and endedAt the
	// first moment the upstream was known to be over (zero until then).
	clock    bool
	now      func() time.Time
	openedAt time.Time
	endedAt  time.Time
}

// newMeter measures a session opened at openedAt. clock is whether the
// provider bills this session by its wall clock rather than by its audio.
func newMeter(config contract.RealtimeSessionConfig, clock bool, openedAt time.Time, now func() time.Time) *meter {
	m := &meter{inputRate: bytesPerSecond(config.InputAudioFormat), clock: clock, now: now, openedAt: openedAt}
	if config.OutputAudioFormat != nil {
		m.outputRate = bytesPerSecond(*config.OutputAudioFormat)
	}
	return m
}

// end stops the session clock at the first moment the upstream is known to be
// over: its connection ended, or Kaana closed it. Later calls change nothing.
func (m *meter) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.endedAt.IsZero() {
		m.endedAt = m.now()
	}
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

// units is the session's measured totals so far. A session billed by its
// clock reports the clock up to now if the upstream is still open: the
// session reads its measurement once, as it settles, immediately before it
// closes the upstream (internal/realtime).
func (m *meter) units() []contract.UsageQuantity {
	m.mu.Lock()
	defer m.mu.Unlock()
	units := []contract.UsageQuantity{}
	if m.clock {
		end := m.endedAt
		if end.IsZero() {
			end = m.now()
		}
		if open := end.Sub(m.openedAt); open > 0 {
			units = append(units, contract.UsageQuantity{Unit: contract.UnitSessionMilliseconds, Quantity: int((open + time.Millisecond - 1) / time.Millisecond)})
		}
		if m.textInputs > 0 {
			units = append(units, contract.UsageQuantity{Unit: contract.UnitRequests, Quantity: m.textInputs})
		}
		return units
	}
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
