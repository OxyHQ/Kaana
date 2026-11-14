package realtime_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openairealtime"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// The emulated live adapters (Kaana ADR 0012 §2) on their fakes, for
// TestLiveContract: OpenAI's Realtime API and xAI's Voice Agent API, one
// adapter in two dialects, each turn handed over by the voice's call to the
// `delegate` tool and answered by that call's function_call_output.

// openAIRealtimeLiveBench is OpenAI's Realtime API. It bills the tokens each
// response.done reports and the transcription model's minutes: the fake
// transcribes each turn in 1.5 s and reports the usage below on the voice's
// answer, its only billed response.
func openAIRealtimeLiveBench(t *testing.T) liveBench {
	upstream := fake.New(t)
	adapter, err := openairealtime.New(openairealtime.Config{
		Declarations: []provider.KeyDeclaration{{KeyID: "key_a", Secret: fake.Key}, {KeyID: "key_b", Secret: fake.SecondKey}},
		HTTPClient:   upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rates := []string{}
	for _, unit := range []contract.UsageUnit{
		contract.UnitAudioInputTokens, contract.UnitAudioOutputTokens, contract.UnitCachedAudioInputTokens, contract.UnitCachedInputTokens,
		contract.UnitInputTokens, contract.UnitInputTranscriptionMilliseconds, contract.UnitOutputTokens,
	} {
		rates = append(rates, fmt.Sprintf(`{"unit":%q,"amountPerUnit":1000}`, unit))
	}
	return emulatedLiveBench(t, upstream, served{adapter: adapter, slug: openairealtime.Slug, upstreamModel: "gpt-realtime-2.1",
		modelReference: "openai/gpt-realtime-2.1@observed-2026-09-30", rates: strings.Join(rates, ",")}, true,
		[]contract.UsageQuantity{
			{Unit: contract.UnitAudioInputTokens, Quantity: 16}, {Unit: contract.UnitAudioOutputTokens, Quantity: 6},
			{Unit: contract.UnitCachedAudioInputTokens, Quantity: 4}, {Unit: contract.UnitCachedInputTokens, Quantity: 3},
			{Unit: contract.UnitInputTokens, Quantity: 7}, {Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: 4_500},
			{Unit: contract.UnitOutputTokens, Quantity: 5},
		}, contract.UsageProviderReported)
}

// xAIRealtimeLiveBench is xAI's Voice Agent API under server_vad, which bills
// the session's wall clock: the adapter's clock reads 42.001 s after the
// handshake, so the session settles 42 001 ms, measured by Kaana, and nothing
// for the function outputs and response.create the emulation sends.
func xAIRealtimeLiveBench(t *testing.T) liveBench {
	upstream := fake.NewXAI(t)
	opened := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var once sync.Once
	now := func() time.Time {
		at := opened.Add(42_001 * time.Millisecond)
		once.Do(func() { at = opened })
		return at
	}
	adapter, err := openairealtime.NewXAI(openairealtime.Config{
		Declarations: []provider.KeyDeclaration{{KeyID: "key_a", Secret: fake.XAIKey}, {KeyID: "key_b", Secret: fake.XAISecondKey}},
		HTTPClient:   upstream.Client(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return emulatedLiveBench(t, upstream, served{adapter: adapter, slug: openairealtime.XAISlug, upstreamModel: "grok-voice-3.0",
		modelReference: "x-ai/grok-voice-3.0@observed-2026-10-16", rates: `{"unit":"requests","amountPerUnit":5},{"unit":"session_milliseconds","amountPerUnit":7}`}, false,
		[]contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 42_001}}, contract.UsageOxyMeasured)
}

// emulatedLiveBench serves one dialect's adapter; the wire is the session it
// opened on the fake.
func emulatedLiveBench(t *testing.T, upstream *fake.Upstream, adapter served, openAI bool, units []contract.UsageQuantity, source contract.UsageSource) liveBench {
	conns := make(chan *fake.Conn, 1)
	upstream.Script = func(conn *fake.Conn) { conns <- conn }
	return liveBench{served: adapter, wire: func(t *testing.T) liveWire {
		select {
		case conn := <-conns:
			return &emulatedLiveWire{upstream: upstream, conn: conn, openAI: openAI, units: units, source: source}
		case <-time.After(10 * time.Second):
			t.Fatal("the adapter never configured a session on the fake")
			return nil
		}
	}}
}

// emulatedLiveWire plays a speech-to-speech voice with tools: the caller's
// turns committed by its server_vad and transcribed, the voice's call to
// `delegate`, its spoken answers, and the function outputs Kaana sends back.
type emulatedLiveWire struct {
	upstream *fake.Upstream
	conn     *fake.Conn
	openAI   bool
	units    []contract.UsageQuantity
	source   contract.UsageSource

	mu    sync.Mutex
	turns int
	calls int
	said  int
}

func (w *emulatedLiveWire) next(counter *int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	*counter++
	return *counter
}

func (w *emulatedLiveWire) Hear(text string) {
	item := fmt.Sprintf("item_turn_%d", w.next(&w.turns))
	cut := strings.LastIndex(text, " ")
	w.conn.SendAll([]fake.Event{
		{"type": "input_audio_buffer.speech_started", "item_id": item, "audio_start_ms": 0},
		{"type": "input_audio_buffer.speech_stopped", "item_id": item, "audio_end_ms": 1500},
		{"type": "input_audio_buffer.committed", "item_id": item},
	})
	completed := fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": item, "content_index": 0, "transcript": text}
	if w.openAI {
		// OpenAI streams the transcript as deltas, then completes it with the
		// transcription model's usage.
		w.conn.SendAll([]fake.Event{
			{"type": "conversation.item.input_audio_transcription.delta", "item_id": item, "content_index": 0, "delta": text[:cut]},
			{"type": "conversation.item.input_audio_transcription.delta", "item_id": item, "content_index": 0, "delta": text[cut:]},
		})
		completed["usage"] = fake.TranscriptionDuration(1.5)
	} else {
		// xAI sends its running transcript whole each time.
		w.conn.SendAll([]fake.Event{
			{"type": "conversation.item.input_audio_transcription.updated", "item_id": item, "transcript": text[:cut]},
			{"type": "conversation.item.input_audio_transcription.updated", "item_id": item, "transcript": text},
		})
	}
	w.conn.Send(completed)
}

func (w *emulatedLiveWire) Delegate() contract.RealtimeDelegationID {
	call := w.next(&w.calls)
	id := fmt.Sprintf("call_delegate_%d", call)
	w.conn.SendAll(w.upstream.Delegate(fmt.Sprintf("resp_delegate_%d", call), id, "the person's request", nil))
	return contract.RealtimeDelegationID(id)
}

func (w *emulatedLiveWire) Speak(text string, audio []byte) {
	response := fmt.Sprintf("resp_said_%d", w.next(&w.said))
	item := "item_" + response
	cut := strings.Index(text, " ")
	done := fake.Event{"id": response, "object": "realtime.response", "status": "completed", "output": []fake.Event{}}
	if w.openAI {
		done["usage"] = fake.Usage(10, 20, 3, 4, 5, 6)
	}
	part := func(event fake.Event) fake.Event {
		event["response_id"], event["item_id"], event["output_index"], event["content_index"] = response, item, 0, 0
		return event
	}
	w.conn.SendAll([]fake.Event{
		{"type": "response.created", "response": fake.Event{"id": response, "object": "realtime.response", "status": "in_progress", "output": []fake.Event{}}},
		part(fake.Event{"type": "response.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(audio)}),
		part(fake.Event{"type": "response.output_audio_transcript.delta", "delta": text[:cut]}),
		part(fake.Event{"type": "response.output_audio_transcript.delta", "delta": text[cut:]}),
		part(fake.Event{"type": "response.output_audio.done"}),
		part(fake.Event{"type": "response.output_audio_transcript.done", "transcript": text}),
		{"type": "response.done", "response": done},
	})
}

func (w *emulatedLiveWire) AudioIn() []string {
	var audio []string
	for _, event := range w.upstream.Received() {
		if event.Type() == "input_audio_buffer.append" {
			data, _ := event["audio"].(string)
			audio = append(audio, data)
		}
	}
	return audio
}

// outputs is every function output the provider received for a call.
func (w *emulatedLiveWire) outputs(id contract.RealtimeDelegationID) []string {
	var outputs []string
	for _, event := range w.upstream.Received() {
		item, _ := event["item"].(map[string]any)
		if event.Type() == "conversation.item.create" && item["type"] == "function_call_output" && item["call_id"] == string(id) {
			output, _ := item["output"].(string)
			outputs = append(outputs, output)
		}
	}
	return outputs
}

// answer splits one output into its context and the answer the voice speaks.
func answer(output string) (context, spoken string) {
	if rest, framed := strings.CutPrefix(output, openairealtime.LiveContextLabel+" "); framed {
		context, spoken, _ = strings.Cut(rest, "\n\n")
		return context, spoken
	}
	return "", output
}

func (w *emulatedLiveWire) Spoken(id contract.RealtimeDelegationID) string {
	var spoken []string
	for _, output := range w.outputs(id) {
		if output != openairealtime.LiveDroppedOutput {
			_, said := answer(output)
			spoken = append(spoken, said)
		}
	}
	return strings.Join(spoken, " ")
}

func (w *emulatedLiveWire) Context(id contract.RealtimeDelegationID) string {
	var contexts []string
	for _, output := range w.outputs(id) {
		if context, _ := answer(output); context != "" {
			contexts = append(contexts, context)
		}
	}
	return strings.Join(contexts, " ")
}

func (w *emulatedLiveWire) Cancelled(id contract.RealtimeDelegationID) bool {
	for _, output := range w.outputs(id) {
		if output == openairealtime.LiveDroppedOutput {
			return true
		}
	}
	return false
}

func (w *emulatedLiveWire) Settles() ([]contract.UsageQuantity, contract.UsageSource) {
	return w.units, w.source
}
