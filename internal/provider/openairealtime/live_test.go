package openairealtime

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// A live session emulated on both OpenAI-Realtime-shaped wires (Kaana ADR 0012
// §2): every test runs on OpenAI's and on xAI's, which differ only where their
// dialects do.

const livePersona = "Eres Alia. Hablas con calma."

// liveConfig is a live session as a client opens it: a persona, a voice, one
// format both ways, server_vad that the caller asked not to answer by itself
// (the voice answers every turn on a live session anyway, by delegating), the
// caller's language, and no other field a live session refuses.
func liveConfig(voice string) contract.RealtimeSessionConfig {
	return contract.RealtimeSessionConfig{
		Instructions:      pointer(livePersona),
		Voice:             pointer(voice),
		InputAudioFormat:  contract.RealtimePCM16,
		OutputAudioFormat: pointer(contract.RealtimePCM16),
		TurnDetection: contract.RealtimeTurnDetection{
			Type: contract.TurnDetectionServerVAD, Threshold: pointer(0.5), SilenceDurationMs: pointer(500),
			CreateResponse: pointer(true), InterruptResponse: pointer(true),
		},
		InputAudioTranscription: &contract.RealtimeInputTranscription{Language: pointer("es-ES")},
		Delegation:              &contract.RealtimeDelegationConfig{Acknowledgement: contract.RealtimeAcknowledgementAuto},
	}
}

type liveWire struct {
	name    string
	dialect *dialect
	fake    func(testing.TB) *fake.Upstream
	route   provider.Route
	voice   string
	adapter func(t *testing.T, upstream *fake.Upstream, now func() time.Time) *Adapter
}

var liveWires = []liveWire{
	{"openai", openAIDialect, fake.New, testRoute(), "marin", func(t *testing.T, upstream *fake.Upstream, now func() time.Time) *Adapter {
		t.Helper()
		adapter, err := New(Config{Declarations: []provider.KeyDeclaration{{KeyID: "key_realtime", Secret: fake.Key}}, HTTPClient: upstream.Client(), Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}},
	{"xai", xAIDialect, fake.NewXAI, xaiRoute(), "eve", xaiAdapterWithClock},
}

func (w liveWire) open(t *testing.T, upstream *fake.Upstream, config contract.RealtimeSessionConfig, now func() time.Time) provider.RealtimeUpstream {
	t.Helper()
	request := provider.RealtimeOpenRequest{RequestID: requestID, Route: w.route, Kind: contract.RealtimeLive, Config: config}
	session, _, err := w.adapter(t, upstream, now).Open(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// liveRelayed collects relayed events until one of type last, returning the
// event types in order and the units carried, events or not.
func liveRelayed(t *testing.T, session provider.RealtimeUpstream, last contract.RealtimeEventType) ([]provider.RealtimeUpstreamEvent, []contract.RealtimeEventType, map[contract.UsageUnit]int) {
	t.Helper()
	var events []provider.RealtimeUpstreamEvent
	var types []contract.RealtimeEventType
	units := map[contract.UsageUnit]int{}
	for {
		event := next(t, session)
		for _, unit := range event.Units {
			units[unit.Unit] += unit.Quantity
		}
		if event.Event == nil {
			continue
		}
		events = append(events, event)
		types = append(types, event.Event.EventType())
		if event.Event.EventType() == last {
			return events, types, units
		}
	}
}

func reply(id, text string, final bool, verbatim ...string) *contract.RealtimeDelegationReplyCommand {
	return &contract.RealtimeDelegationReplyCommand{SchemaVersion: 1, RequestID: requestID, CommandID: contract.RealtimeCommandID("cmd_reply_" + text[:3]), DelegationID: contract.RealtimeDelegationID(id), Text: text, Verbatim: verbatim, Final: final}
}

// transcribedTurn is one turn of the caller's speech, committed and
// transcribed, in the order both providers send it.
func transcribedTurn(item, transcript string) []fake.Event {
	return []fake.Event{
		{"type": "input_audio_buffer.speech_started", "item_id": item, "audio_start_ms": 100},
		{"type": "input_audio_buffer.speech_stopped", "item_id": item, "audio_end_ms": 900},
		{"type": "input_audio_buffer.committed", "item_id": item},
		{"type": "conversation.item.input_audio_transcription.completed", "item_id": item, "content_index": 0, "transcript": transcript},
	}
}

// spokenAnswer is the voice speaking an answer: a response with audio and its
// transcript, the verbatim span read with its marks.
func spokenAnswer(responseID string, usage fake.Event) []fake.Event {
	audio := base64.StdEncoding.EncodeToString(make([]byte, 4800))
	done := fake.Event{"id": responseID, "object": "realtime.response", "status": "completed", "output": []fake.Event{}}
	if usage != nil {
		done["usage"] = usage
	}
	return []fake.Event{
		{"type": "response.created", "response": fake.Event{"id": responseID, "object": "realtime.response", "status": "in_progress", "output": []fake.Event{}}},
		{"type": "response.output_audio.delta", "response_id": responseID, "item_id": "item_answer", "output_index": 0, "content_index": 0, "delta": audio},
		{"type": "response.output_audio_transcript.delta", "response_id": responseID, "item_id": "item_answer", "output_index": 0, "content_index": 0, "delta": "Son las ⟦10:45⟧."},
		{"type": "response.output_audio_transcript.delta", "response_id": responseID, "item_id": "item_answer", "output_index": 0, "content_index": 0, "delta": "⟧"},
		{"type": "response.output_audio.done", "response_id": responseID, "item_id": "item_answer", "output_index": 0, "content_index": 0},
		{"type": "response.output_audio_transcript.done", "response_id": responseID, "item_id": "item_answer", "output_index": 0, "content_index": 0, "transcript": "Son las ⟦10:45⟧. Buen día."},
		{"type": "response.done", "response": done},
	}
}

// TestALiveSessionIsConfiguredAsAConversationThatDelegates pins the
// configuring session.update of a live session on each wire: the shared
// preamble before the persona, exactly the one delegate tool under tool
// choice auto (nothing on xAI, whose only choice it is), spoken answers, the
// caller's speech transcribed in the caller's language even when the caller
// did not ask, and a turn detection that answers every turn and is
// interruptible even when the caller asked otherwise on OpenAI (xAI's has no
// such switch).
func TestALiveSessionIsConfiguredAsAConversationThatDelegates(t *testing.T) {
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			config := liveConfig(wire.voice)
			if wire.dialect == openAIDialect {
				config.TurnDetection.CreateResponse, config.TurnDetection.InterruptResponse = pointer(false), pointer(false)
			}
			upstream := wire.fake(t)
			wire.open(t, upstream, config, nil)
			session := sessionOf(t, upstream.Received()[0])
			if session["instructions"] != provider.LivePreamble+"\n\n"+livePersona {
				t.Errorf("instructions = %q", session["instructions"])
			}
			tools, _ := session["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("tools = %v; want the delegate tool alone", session["tools"])
			}
			tool := tools[0].(map[string]any)
			wantParameters := `{"properties":{"request":{"description":"The person's request, in their own words.","type":"string"}},"required":["request"],"type":"object"}`
			if tool["type"] != "function" || tool["name"] != "delegate" || asJSON(t, tool["parameters"]) != wantParameters {
				t.Errorf("tool = %s", asJSON(t, tool))
			}
			if !strings.Contains(tool["description"].(string), "one very short acknowledgement") {
				t.Errorf("acknowledgement auto: the tool does not allow one: %q", tool["description"])
			}
			switch wire.dialect {
			case openAIDialect:
				if session["tool_choice"] != "auto" || asJSON(t, session["output_modalities"]) != `["audio"]` {
					t.Errorf("tool_choice %v, output_modalities %v", session["tool_choice"], session["output_modalities"])
				}
				want := `{"format":{"rate":24000,"type":"audio/pcm"},"transcription":{"model":"gpt-transcribe"},"turn_detection":{"create_response":true,"interrupt_response":true,"silence_duration_ms":500,"threshold":0.5,"type":"server_vad"}}`
				if got := asJSON(t, session["audio"].(map[string]any)["input"]); got != want {
					t.Errorf("audio.input = %s\nwant %s", got, want)
				}
				if got := asJSON(t, session["audio"].(map[string]any)["output"]); got != `{"format":{"rate":24000,"type":"audio/pcm"},"voice":"marin"}` {
					t.Errorf("audio.output = %s", got)
				}
			case xAIDialect:
				if _, named := session["tool_choice"]; named || session["voice"] != "eve" {
					t.Errorf("tool_choice %v, voice %v", session["tool_choice"], session["voice"])
				}
				if got := asJSON(t, session["turn_detection"]); got != `{"silence_duration_ms":500,"threshold":0.5,"type":"server_vad"}` {
					t.Errorf("turn_detection = %s", got)
				}
				if got := asJSON(t, session["audio"].(map[string]any)["input"].(map[string]any)["transcription"]); got != `{"language_hint":"es-ES","model":"grok-transcribe"}` {
					t.Errorf("transcription = %s", got)
				}
			}

			// A caller that asked for no transcription is transcribed all the
			// same, and acknowledgement none tells the voice to say nothing
			// before it hands the turn over.
			config = liveConfig(wire.voice)
			config.InputAudioTranscription = nil
			config.Delegation.Acknowledgement = contract.RealtimeAcknowledgementNone
			upstream = wire.fake(t)
			wire.open(t, upstream, config, nil)
			session = sessionOf(t, upstream.Received()[0])
			if _, transcribes := session["audio"].(map[string]any)["input"].(map[string]any)["transcription"]; !transcribes {
				t.Error("a live session that named no transcription does not transcribe the caller")
			}
			description := session["tools"].([]any)[0].(map[string]any)["description"].(string)
			if !strings.Contains(description, "without saying anything first") || strings.Contains(description, "acknowledgement") {
				t.Errorf("acknowledgement none: %q", description)
			}
		})
	}
}

// TestALiveTurnIsHandedOverAndItsAnswerSpoken drives one whole turn on each
// wire: the caller's words become delegation.requested and nothing of the
// conversation underneath is relayed; the reply, sent in two parts around a
// context with a verbatim span, reaches the provider once, as the call's
// output, context first and the span marked, followed by response.create; the
// voice's answer is relayed as audio and a transcript without the marks. What
// the provider bills is settled exactly: OpenAI's reported tokens and
// transcription, and on xAI nothing but its session clock.
func TestALiveTurnIsHandedOverAndItsAnswerSpoken(t *testing.T) {
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			var callUsage, answerUsage fake.Event
			turn := transcribedTurn("item_1", "¿Qué hora es?")
			if wire.dialect == openAIDialect {
				callUsage, answerUsage = fake.Usage(10, 20, 0, 0, 5, 0), fake.Usage(30, 0, 0, 0, 4, 40)
				turn[3]["usage"] = fake.TranscriptionDuration(1.2)
			}
			upstream := wire.fake(t)
			outputs := make(chan string, 1)
			upstream.Script = func(conn *fake.Conn) {
				conn.SendAll(turn)
				conn.SendAll(upstream.Delegate("resp_call", "call_1", "what time is it", callUsage))
				outputs <- conn.ExpectOutput("call_1", true)
				conn.SendAll(spokenAnswer("resp_answer", answerUsage))
			}
			clock := newClock()
			session := wire.open(t, upstream, liveConfig(wire.voice), clock.Now)

			events, types, units := liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			want := []contract.RealtimeEventType{contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventDelegationRequestedType}
			if !reflect.DeepEqual(types, want) {
				t.Fatalf("relayed %v, want %v", types, want)
			}
			requested := events[2].Event.(*contract.RealtimeDelegationRequestedEvent)
			if requested.DelegationID != "call_1" || requested.Text != "¿Qué hora es?" {
				t.Errorf("delegation = %+v; want call_1 with the caller's words", requested)
			}

			send(t, session, reply("call_1", "Son las 10:45.", false, "10:45"))
			send(t, session, &contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_context", DelegationID: "call_1", Text: "Checked the clock."})
			send(t, session, reply("call_1", "Buen día.", true))
			if output := <-outputs; output != liveContextLabel+" Checked the clock.\n\nSon las ⟦10:45⟧. Buen día." {
				t.Errorf("the call's output = %q", output)
			}

			answer, types, answerUnits := liveRelayed(t, session, contract.RealtimeEventTranscriptDoneType)
			want = []contract.RealtimeEventType{contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDoneType}
			if !reflect.DeepEqual(types, want) {
				t.Fatalf("the answer relayed %v, want %v", types, want)
			}
			if delta := answer[1].Event.(*contract.RealtimeTranscriptDeltaEvent); delta.Text != "Son las 10:45." || delta.Source != contract.RealtimeTranscriptOutput {
				t.Errorf("transcript delta = %+v", delta)
			}
			if done := answer[2].Event.(*contract.RealtimeTranscriptDoneEvent); done.Transcript != "Son las 10:45. Buen día." {
				t.Errorf("transcript = %q", done.Transcript)
			}
			for unit, quantity := range answerUnits {
				units[unit] += quantity
			}
			// The response.done that ends the answer carries its usage after
			// the transcript: read it, units only, before closing.
			if wire.dialect == openAIDialect {
				event := next(t, session)
				if event.Event != nil {
					t.Fatalf("relayed %s after the answer", event.Event.EventType())
				}
				for _, unit := range event.Units {
					units[unit.Unit] += unit.Quantity
				}
			}

			received := upstream.ReceivedTypes()
			if !reflect.DeepEqual(received, []string{"session.update", "conversation.item.create", "response.create"}) {
				t.Errorf("the provider received %v", received)
			}
			switch wire.dialect {
			case openAIDialect:
				wantUnits := map[contract.UsageUnit]int{
					contract.UnitInputTranscriptionMilliseconds: 1200,
					contract.UnitInputTokens:                    40, contract.UnitAudioInputTokens: 20, contract.UnitCachedInputTokens: 0, contract.UnitCachedAudioInputTokens: 0,
					contract.UnitOutputTokens: 9, contract.UnitAudioOutputTokens: 40,
				}
				if !reflect.DeepEqual(units, wantUnits) {
					t.Errorf("OpenAI units = %v, want %v", units, wantUnits)
				}
			case xAIDialect:
				if len(units) != 0 {
					t.Errorf("xAI events carried units %v; xAI is metered", units)
				}
				clock.advance(90 * time.Second)
				measured := session.(provider.RealtimeMeter).Measured()
				if !reflect.DeepEqual(measured, []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 90_000}}) {
					t.Errorf("xAI measured %v; want the session clock alone, no request for the function output", measured)
				}
			}
		})
	}
}

// TestALiveDelegationWaitsForItsTurnsTranscript: OpenAI transcribes the
// caller asynchronously, so the voice can call delegate before the turn's
// transcript arrives. The delegation is handed over when it does, with the
// caller's words, never with the voice's paraphrase while the words are on
// their way; and a turn that cannot be transcribed, or a delegation without
// any turn, is handed over with the voice's request.
func TestALiveDelegationWaitsForItsTurnsTranscript(t *testing.T) {
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			turn := transcribedTurn("item_1", "Reserva mesa para dos.")
			upstream := wire.fake(t)
			upstream.Script = func(conn *fake.Conn) {
				conn.SendAll(turn[:3])
				conn.SendAll(upstream.Delegate("resp_1", "call_1", "book a table", nil))
				conn.Send(turn[3])
				// A turn the provider failed to transcribe.
				conn.SendAll(transcribedTurn("item_2", "")[:3])
				conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.failed", "item_id": "item_2", "content_index": 0, "error": fake.Event{"type": "transcription_error", "message": "no speech"}})
				conn.SendAll(upstream.Delegate("resp_2", "call_2", "and for three?", nil))
				// A delegation with no turn of speech at all.
				conn.SendAll(upstream.Delegate("resp_3", "call_3", "anything else?", nil))
			}
			session := wire.open(t, upstream, liveConfig(wire.voice), nil)
			events, types, _ := liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			if want := []contract.RealtimeEventType{contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventDelegationRequestedType}; !reflect.DeepEqual(types, want) {
				t.Fatalf("relayed %v, want the transcript before the delegation %v", types, want)
			}
			if got := events[2].Event.(*contract.RealtimeDelegationRequestedEvent); got.Text != "Reserva mesa para dos." {
				t.Errorf("delegation text = %q; want the caller's words", got.Text)
			}
			events, types, _ = liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			if want := []contract.RealtimeEventType{contract.RealtimeEventErrorType, contract.RealtimeEventDelegationRequestedType}; !reflect.DeepEqual(types, want) {
				t.Fatalf("an untranscribed turn relayed %v, want %v", types, want)
			}
			if got := events[1].Event.(*contract.RealtimeDelegationRequestedEvent); got.DelegationID != "call_2" || got.Text != "and for three?" {
				t.Errorf("an untranscribed turn's delegation = %+v; want the voice's request", got)
			}
			events, _, _ = liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			if got := events[0].Event.(*contract.RealtimeDelegationRequestedEvent); got.DelegationID != "call_3" || got.Text != "anything else?" {
				t.Errorf("a delegation with no turn = %+v; want the voice's request", got)
			}
		})
	}
}

// TestACancelledDelegationIsClosedUnspoken: delegation.cancel closes the
// voice's call with an output saying the request was dropped, and no
// response.create follows it, so the voice says nothing for it: the next
// client event the provider reads is the caller's audio.
func TestACancelledDelegationIsClosedUnspoken(t *testing.T) {
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			upstream := wire.fake(t)
			outputs := make(chan string, 1)
			upstream.Script = func(conn *fake.Conn) {
				conn.SendAll(upstream.Delegate("resp_1", "call_1", "cancel me", nil))
				outputs <- conn.ExpectOutput("call_1", false)
				conn.Expect("input_audio_buffer.append")
			}
			session := wire.open(t, upstream, liveConfig(wire.voice), nil)
			liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			send(t, session, reply("call_1", "Never sent.", false))
			send(t, session, &contract.RealtimeDelegationCancelCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_cancel", DelegationID: "call_1"})
			if output := <-outputs; output != liveDroppedOutput {
				t.Errorf("the cancelled call's output = %q", output)
			}
			send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_audio", Data: base64.StdEncoding.EncodeToString(make([]byte, 480))})
			_ = session.Close()
			upstream.WaitClosed(t)
			if got := upstream.ReceivedTypes(); !reflect.DeepEqual(got, []string{"session.update", "conversation.item.create", "input_audio_buffer.append"}) {
				t.Errorf("the provider received %v; want no response.create for a cancelled delegation", got)
			}
		})
	}
}

// TestANewerDelegationStartsItsOwnAnswer: a reply begun for a delegation the
// voice then superseded with another call never reaches the newer call's
// output (the session supersedes; the adapter keeps one answer, the newest).
func TestANewerDelegationStartsItsOwnAnswer(t *testing.T) {
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			upstream := wire.fake(t)
			outputs := make(chan string, 1)
			second := make(chan struct{})
			upstream.Script = func(conn *fake.Conn) {
				conn.SendAll(upstream.Delegate("resp_1", "call_1", "first", nil))
				<-second
				conn.SendAll(upstream.Delegate("resp_2", "call_2", "second", nil))
				outputs <- conn.ExpectOutput("call_2", true)
			}
			session := wire.open(t, upstream, liveConfig(wire.voice), nil)
			liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			send(t, session, reply("call_1", "The first answer.", false))
			close(second)
			liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			send(t, session, reply("call_2", "The second answer.", true))
			if output := <-outputs; output != "The second answer." {
				t.Errorf("the newer call's output = %q", output)
			}
		})
	}
}

// TestALiveSessionExposesNoConversationCommand: the commands of the
// conversation the emulation runs on are refused, unsent, naming `type`,
// exactly as a provider with native delegation cannot serve them; the
// caller's audio is the control, and it is sent.
func TestALiveSessionExposesNoConversationCommand(t *testing.T) {
	user, text, responseID := contract.RealtimeItemRole("user"), "hola", contract.RealtimeResponseID("resp_1")
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			upstream := wire.fake(t)
			session := wire.open(t, upstream, liveConfig(wire.voice), nil)
			for _, command := range []contract.RealtimeCommand{
				&contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_item", Item: contract.RealtimeConversationItem{
					Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputTextPart, Text: &text}}}},
				&contract.RealtimeItemDeleteCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_delete", ItemID: "item_1"},
				&contract.RealtimeItemTruncateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_truncate", ItemID: "item_1", AudioEndMs: 10},
				&contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"},
				&contract.RealtimeResponseCancelCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_cancel", ResponseID: &responseID},
			} {
				var unsupported provider.ErrUnsupported
				if err := session.Send(context.Background(), command); !errors.As(err, &unsupported) || unsupported.Param != "type" || strings.Contains(unsupported.Detail, wire.dialect.name) {
					t.Errorf("%s = %v; want a refusal naming type and no provider", command.CommandType(), err)
				}
			}
			send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_audio", Data: base64.StdEncoding.EncodeToString(make([]byte, 480))})
			_ = session.Close()
			upstream.WaitClosed(t)
			if got := upstream.ReceivedTypes(); !reflect.DeepEqual(got, []string{"session.update", "input_audio_buffer.append"}) {
				t.Errorf("the provider received %v", got)
			}
		})
	}
}

// TestALiveDelegationOnXAIsCapturedTurns replays the measured grok-voice-3.0
// sessions (testdata, 2026-10-16) as live sessions, with the voice's delegate
// call spliced into the response xAI created for each turn, right after that
// turn's completed transcript: the order xAI sends a response's output in.
// Where the answer to "Hello Alia." ended before the caller went on, each turn
// is its own delegation with its own words; where the caller talked over it,
// xAI dropped that response (no call), and the one delegation holds both
// turns' words, each once, although xAI's transcript of the second repeated
// the first.
func TestALiveDelegationOnXAIsCapturedTurns(t *testing.T) {
	const capital = "Please tell me, in one short sentence, what is the capital of France?"
	for _, replay := range []struct {
		name string
		// calls is the turn (index of its completed transcript) each call is
		// spliced after.
		calls []int
		want  []string
	}{
		{"xai-ended-turn.jsonl", []int{0, 1}, []string{"Hello Alia.", capital}},
		{"xai-interrupted-turn.jsonl", []int{1}, []string{"Hello Alia. " + capital}},
	} {
		t.Run(replay.name, func(t *testing.T) {
			upstream := fake.NewXAI(t)
			var spliced []fake.Event
			response, completed := "", 0
			for _, event := range capture(t, replay.name) {
				spliced = append(spliced, event)
				switch event.Type() {
				case "response.created":
					response = event["response"].(map[string]any)["id"].(string)
				case "conversation.item.input_audio_transcription.completed":
					for index, turn := range replay.calls {
						if turn == completed {
							spliced = append(spliced, upstream.DelegateCall(response, "call_"+string(rune('a'+index)), "a paraphrase")...)
						}
					}
					completed++
				}
			}
			upstream.Script = func(conn *fake.Conn) { conn.SendAll(spliced) }
			config := liveConfig("eve")
			config.InputAudioTranscription.Language = pointer("en-US")
			session := liveWires[1].open(t, upstream, config, nil)
			for index, want := range replay.want {
				events, _, _ := liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
				got := events[len(events)-1].Event.(*contract.RealtimeDelegationRequestedEvent)
				if got.DelegationID != contract.RealtimeDelegationID("call_"+string(rune('a'+index))) || got.Text != want {
					t.Errorf("delegation %d = %+v; want %q", index, got, want)
				}
			}
		})
	}
}

// TestAHeldDelegationGoesWhenTheCallerTalksOn: a call held for a transcript
// that has not come is handed over, with the voice's request, as soon as the
// caller starts talking again, rather than taking the next turn's words.
func TestAHeldDelegationGoesWhenTheCallerTalksOn(t *testing.T) {
	for _, wire := range liveWires {
		t.Run(wire.name, func(t *testing.T) {
			upstream := wire.fake(t)
			upstream.Script = func(conn *fake.Conn) {
				conn.SendAll(transcribedTurn("item_1", "")[:3])
				conn.SendAll(upstream.Delegate("resp_1", "call_1", "the weather", nil))
				conn.SendAll(transcribedTurn("item_2", "y mañana"))
			}
			session := wire.open(t, upstream, liveConfig(wire.voice), nil)
			events, types, _ := liveRelayed(t, session, contract.RealtimeEventDelegationRequestedType)
			if want := []contract.RealtimeEventType{contract.RealtimeEventDelegationRequestedType}; !reflect.DeepEqual(types, want) {
				t.Fatalf("relayed %v, want %v before the next turn's words", types, want)
			}
			if got := events[0].Event.(*contract.RealtimeDelegationRequestedEvent); got.Text != "the weather" {
				t.Errorf("delegation text = %q; want the voice's request", got.Text)
			}
		})
	}
}
