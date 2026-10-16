package openairealtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Input transcription inside a conversation (contract set 4.1.0, Alia ADR
// 0017): the caller's speech, transcribed by the deployment's declared
// transcription model, relayed as transcript events with source input_audio
// and settled under the unit the provider bills it in.

func transcribed(config contract.RealtimeSessionConfig, transcription contract.RealtimeInputTranscription) contract.RealtimeSessionConfig {
	config.InputAudioTranscription = &transcription
	return config
}

func collectUntil(t *testing.T, session provider.RealtimeUpstream, last contract.RealtimeEventType) []provider.RealtimeUpstreamEvent {
	t.Helper()
	var events []provider.RealtimeUpstreamEvent
	for len(events) == 0 || events[len(events)-1].Event == nil || events[len(events)-1].Event.EventType() != last {
		events = append(events, next(t, session))
	}
	return events
}

func sessionOf(t *testing.T, event fake.Event) map[string]any {
	t.Helper()
	session, ok := event["session"].(map[string]any)
	if !ok {
		t.Fatalf("session.update carries no session: %v", event)
	}
	return session
}

// TestOpenAITranscribesTheCallersSpeechAndBillsItsASRModel drives one spoken
// turn with input transcription through OpenAI's wire: the configuring
// session.update names the declared transcription model, the caller's
// transcript arrives as input_audio transcript events, and the ASR model's
// duration is settled as input_transcription_milliseconds BESIDE the session
// model's own audio tokens — two models, two charges, neither counted twice.
func TestOpenAITranscribesTheCallersSpeechAndBillsItsASRModel(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_user", "previous_item_id": nil})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.delta", "item_id": "item_user", "content_index": 0, "delta": "Hola, "})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.delta", "item_id": "item_user", "content_index": 0, "delta": ""})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.delta", "item_id": "item_user", "content_index": 0, "delta": "¿qué tal?"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "content_index": 0,
			"transcript": "Hola, ¿qué tal?", "usage": fake.TranscriptionDuration(2.3504)})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.segment", "item_id": "item_user", "content_index": 0, "id": "seg_1", "text": "Hola", "speaker": "A", "start": 0, "end": 1})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.failed", "item_id": "item_user_2", "content_index": 0,
			"error": fake.Event{"type": "transcription_error", "code": "audio_unintelligible", "message": "could not transcribe, key " + fake.Key}})
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "output": []any{}, "usage": fake.Usage(10, 120, 0, 0, 5, 200)}})
	}
	session := open(t, upstream, transcribed(conversationConfig(), contract.RealtimeInputTranscription{Prompt: pointer("names: Kaana, Alia")}))

	update := upstream.Received()[0]
	wantTranscription := map[string]any{"model": "gpt-transcribe", "prompt": "names: Kaana, Alia"}
	input := sessionOf(t, update)["audio"].(map[string]any)["input"].(map[string]any)
	if got := input["transcription"]; !reflect.DeepEqual(got, wantTranscription) {
		t.Errorf("audio.input.transcription = %v, want %v", got, wantTranscription)
	}
	if input["turn_detection"] == nil || input["format"] == nil {
		t.Errorf("transcription displaced the input's other fields: %v", input)
	}

	send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_append", Data: "AAAA"})
	events := collectUntil(t, session, contract.RealtimeEventResponseDoneType)
	want := []contract.RealtimeEventType{
		contract.RealtimeEventInputAudioCommittedType, contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDeltaType,
		contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventErrorType, contract.RealtimeEventResponseCreatedType,
		contract.RealtimeEventResponseDoneType,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	for _, event := range events[1:3] {
		delta := event.Event.(*contract.RealtimeTranscriptDeltaEvent)
		if delta.Source != contract.RealtimeTranscriptInput || delta.ItemID != "item_user" || delta.ResponseID != nil || len(event.Units) != 0 {
			t.Errorf("input transcript delta = %+v units %v", delta, event.Units)
		}
	}
	done := events[3].Event.(*contract.RealtimeTranscriptDoneEvent)
	if done.Source != contract.RealtimeTranscriptInput || done.ItemID != "item_user" || done.ContentIndex != 0 || done.Transcript != "Hola, ¿qué tal?" || done.ResponseID != nil {
		t.Errorf("input transcript = %+v", done)
	}
	// 2.3504 s, read to the microsecond and rounded up once: 2 351 ms.
	if want := []contract.UsageQuantity{{Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: 2_351}}; !reflect.DeepEqual(events[3].Units, want) {
		t.Errorf("the transcription was billed %v, want %v", events[3].Units, want)
	}
	failure := events[4].Event.(*contract.RealtimeErrorEvent)
	if failure.Fatal || failure.Error.Code != contract.CodeProviderError || events[4].CommandID != nil {
		t.Errorf("a failed transcription = %+v; it is a non-fatal error naming no command", failure)
	}
	if message := *failure.Error.ProviderError.Message; strings.Contains(message, fake.Key) {
		t.Errorf("the platform key reached the customer: %q", message)
	}
	// The session model's units are its own, untouched by the transcription.
	response := events[6].Event.(*contract.RealtimeResponseDoneEvent)
	for _, quantity := range response.Units {
		if quantity.Unit == contract.UnitInputTranscriptionMilliseconds {
			t.Errorf("response.done carried the transcription's charge: %v", response.Units)
		}
	}
	if !slices.Contains(response.Units, contract.UsageQuantity{Unit: contract.UnitAudioInputTokens, Quantity: 120}) {
		t.Errorf("response.done lost the session model's audio input: %v", response.Units)
	}
}

// TestATranscriptionBilledInTokensEndsTheSession: the declared transcription
// model is billed by duration. A token usage is a charge
// input_transcription_milliseconds does not express, so the session ends
// rather than settle a guess or drop a charge. A duration on the same script
// is the control.
func TestATranscriptionBilledInTokensEndsTheSession(t *testing.T) {
	drive := func(usage fake.Event) error {
		upstream := fake.New(t)
		upstream.Script = func(conn *fake.Conn) {
			conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "content_index": 0, "transcript": "hola", "usage": usage})
		}
		session := open(t, upstream, transcribed(conversationConfig(), contract.RealtimeInputTranscription{}))
		_, err := session.Next(context.Background())
		return err
	}
	var upstream provider.ErrUpstream
	if err := drive(fake.TranscriptionTokens(40, 3, 6)); !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderError || !strings.Contains(upstream.Detail, "tokens") {
		t.Errorf("a token-billed transcription = %v; want the session to end on it", err)
	}
	if err := drive(fake.Event{"type": "duration", "seconds": -1}); !errors.As(err, &upstream) {
		t.Errorf("a negative duration = %v; want it refused as unreadable", err)
	}
	if err := drive(fake.TranscriptionDuration(1.2)); err != nil {
		t.Errorf("the duration control = %v", err)
	}
}

// TestATranscriptNobodyAskedForIsNotRelayedButItsChargeIsSettled: a session
// that did not ask for input transcription relays no input transcript, and if
// the provider reports one anyway, the charge it reports is still settled.
// The same session asking for it mid-session (session.update) relays the next
// one: the update reaches OpenAI with the declared model.
func TestATranscriptNobodyAskedForIsNotRelayedButItsChargeIsSettled(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.delta", "item_id": "item_1", "content_index": 0, "delta": "hi"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_1", "content_index": 0, "transcript": "hi", "usage": fake.TranscriptionDuration(0.5)})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.failed", "item_id": "item_1", "content_index": 0, "error": fake.Event{"type": "x"}})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_2", "content_index": 0, "transcript": "no usage"})
		update := conn.Expect("session.update")
		got := update["session"].(map[string]any)["audio"].(map[string]any)["input"].(map[string]any)["transcription"]
		if want := map[string]any{"model": "gpt-transcribe"}; !reflect.DeepEqual(got, want) {
			t.Errorf("the enabling update sent %v, want %v", got, want)
		}
		conn.Send(fake.Event{"type": "session.updated", "session": update["session"]})
		// OpenAI commits the item, then sends its one transcript.
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_3"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_3", "content_index": 0, "transcript": "now", "usage": fake.TranscriptionDuration(0.25)})
	}
	session := open(t, upstream, conversationConfig())
	silent := next(t, session)
	if silent.Event != nil || !reflect.DeepEqual(silent.Units, []contract.UsageQuantity{{Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: 500}}) {
		t.Fatalf("an unrequested transcription = %+v; want only its charge", silent)
	}
	send(t, session, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_transcribe",
		Config: contract.RealtimeSessionConfigUpdate{InputAudioTranscription: &contract.RealtimeInputTranscription{}}})
	confirmed := next(t, session)
	updated := confirmed.Event.(*contract.RealtimeSessionUpdatedEvent)
	if updated.Config.InputAudioTranscription == nil {
		t.Errorf("the effective configuration lost the transcription: %+v", updated.Config)
	}
	if committed := next(t, session); committed.Event.EventType() != contract.RealtimeEventInputAudioCommittedType {
		t.Fatalf("the commit = %+v", committed)
	}
	if delta := next(t, session); delta.Event.EventType() != contract.RealtimeEventTranscriptDeltaType {
		t.Fatalf("the transcript's delta = %+v", delta)
	}
	relayed := next(t, session)
	done, ok := relayed.Event.(*contract.RealtimeTranscriptDoneEvent)
	if !ok || done.ItemID != "item_3" || !reflect.DeepEqual(relayed.Units, []contract.UsageQuantity{{Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: 250}}) {
		t.Errorf("after enabling, the transcript = %+v", relayed)
	}
}

// TestXAITranscribesInsideItsSessionPrice: xAI's wire names `grok-transcribe`
// and a `language_hint` in its spelling, its running and early "completed"
// transcripts become deltas that only append, its first completed transcript
// after the commit is the item's one transcript.done, a repeat after it is
// dropped, and the session's measurement is exactly what it would be without
// transcription: xAI's pricing has no transcription line.
func TestXAITranscribesInsideItsSessionPrice(t *testing.T) {
	appended := base64.StdEncoding.EncodeToString(make([]byte, 4800)) // 100 ms of PCM16
	script := func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		// Measured from xAI (2026-10-16): a running transcript, a "completed"
		// one at the pause, the final one after the commit, and a repeat.
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.updated", "item_id": "item_user", "transcript": "Hol"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "transcript": "Hola"})
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_user"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "transcript": "Hola, Alia"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "transcript": "Hola, Alia"})
		conn.Send(fake.Event{"type": "input_audio_buffer.cleared"})
	}
	drive := func(config contract.RealtimeSessionConfig) ([]provider.RealtimeUpstreamEvent, []contract.UsageQuantity, *fake.Upstream) {
		upstream := fake.NewXAI(t)
		upstream.Script = script
		session := xaiOpen(t, upstream, config)
		send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_audio", Data: appended})
		events := collectUntil(t, session, contract.RealtimeEventInputAudioClearedType)
		return events, session.(provider.RealtimeMeter).Measured(), upstream
	}

	// The client's own tag, in a region xAI does not list, is sent as xAI's
	// variant of the language.
	events, measured, upstream := drive(transcribed(pushToTalk(), contract.RealtimeInputTranscription{Language: pointer("es-AR")}))
	update := sessionOf(t, upstream.Received()[0])
	got := update["audio"].(map[string]any)["input"].(map[string]any)["transcription"]
	if want := map[string]any{"model": "grok-transcribe", "language_hint": "es-MX"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio.input.transcription = %v, want %v", got, want)
	}
	if update["turn_detection"] == nil || update["voice"] != "eve" {
		t.Errorf("xAI's flat session fields were displaced: %v", update)
	}
	want := []contract.RealtimeEventType{
		contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventInputAudioCommittedType,
		contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventInputAudioClearedType,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	var deltas string
	for _, event := range events {
		if delta, ok := event.Event.(*contract.RealtimeTranscriptDeltaEvent); ok {
			deltas += delta.Text
		}
	}
	done := events[4].Event.(*contract.RealtimeTranscriptDoneEvent)
	if done.Source != contract.RealtimeTranscriptInput || done.ItemID != "item_user" || done.ContentIndex != 0 || done.Transcript != "Hola, Alia" || len(events[4].Units) != 0 {
		t.Errorf("xAI's input transcript = %+v units %v", done, events[4].Units)
	}
	if deltas != done.Transcript {
		t.Errorf("the deltas append %q, the transcript is %q", deltas, done.Transcript)
	}

	// Control: the same session without transcription relays no transcript
	// and is measured identically.
	plainEvents, plainMeasured, _ := drive(pushToTalk())
	if got := eventTypes(plainEvents); !reflect.DeepEqual(got, []contract.RealtimeEventType{contract.RealtimeEventInputAudioCommittedType, contract.RealtimeEventInputAudioClearedType}) {
		t.Errorf("an unrequested xAI transcript was relayed: %v", got)
	}
	if !reflect.DeepEqual(measured, plainMeasured) {
		t.Errorf("transcription changed xAI's measurement: %v, without it %v", measured, plainMeasured)
	}
	for _, quantity := range measured {
		if quantity.Unit == contract.UnitInputTranscriptionMilliseconds {
			t.Errorf("xAI reported a transcription charge its pricing does not have: %v", measured)
		}
	}
}

// TestTheCatalogueStatementIsWhatTheAdapterServes ties the statement
// (providerconfig.RealtimeCapabilities) to this adapter: the published flag is
// set exactly where a dialect transcribes, every published encoding is one the
// wire translates, and the internal parameters are valid.
func TestTheCatalogueStatementIsWhatTheAdapterServes(t *testing.T) {
	for _, d := range []*dialect{openAIDialect, xAIDialect} {
		statement, stated := providerconfig.RealtimeCapabilities(d.slug, d.protocol)
		if !stated {
			t.Fatalf("%s publishes no realtime statement", d.slug)
		}
		if statement.InputAudioTranscription != (d.transcription != nil) {
			t.Errorf("%s publishes inputAudioTranscription %t; its dialect transcribes: %t", d.slug, statement.InputAudioTranscription, d.transcription != nil)
		}
		if !reflect.DeepEqual(statement.SessionKinds, providerconfig.RealtimeSessionKinds(d.slug, d.protocol)) {
			t.Errorf("%s publishes kinds %v", d.slug, statement.SessionKinds)
		}
		for _, format := range append(append([]contract.RealtimeAudioFormat{}, statement.InputAudioFormats...), statement.OutputAudioFormats...) {
			if _, err := wireFormat(format); err != nil {
				t.Errorf("%s publishes %s, which the wire cannot carry", d.slug, format)
			}
		}
		if err := statement.Parameters.Validate(); err != nil {
			t.Errorf("%s states invalid parameters: %v", d.slug, err)
		}
	}
}

// TestTheDialectTranslatesEveryContractValue: every value the contract
// offers is either written to the provider's wire — as it is, or translated
// or emulated (Kaana ADR 0003) — or, when no translation can serve it,
// refused naming its contract field and exactly as Unservable, the check the
// public API runs before a grant, says. Nothing in between.
func TestTheDialectTranslatesEveryContractValue(t *testing.T) {
	yes, no := true, false
	detections := []contract.RealtimeTurnDetection{
		{Type: contract.TurnDetectionNone},
		{Type: contract.TurnDetectionServerVAD, CreateResponse: &yes, InterruptResponse: &yes, Threshold: pointer(0.0)},
		{Type: contract.TurnDetectionServerVAD, CreateResponse: &yes, InterruptResponse: &yes, Threshold: pointer(1.0), SilenceDurationMs: pointer(10_000)},
		{Type: contract.TurnDetectionServerVAD, CreateResponse: &no, InterruptResponse: &yes},
		{Type: contract.TurnDetectionServerVAD, CreateResponse: &yes, InterruptResponse: &no},
		{Type: contract.TurnDetectionSemanticVAD, Eagerness: pointer(contract.RealtimeVADEagerness("low")), CreateResponse: &yes, InterruptResponse: &yes},
	}
	choices := []contract.ToolChoice{
		{Mode: pointer(contract.ToolChoiceAuto)}, {Mode: pointer(contract.ToolChoiceNone)}, {Mode: pointer(contract.ToolChoiceRequired)},
		{Function: &contract.ToolChoiceFunction{Type: "function", Name: "lookup"}},
	}
	modalities := [][]contract.RealtimeOutputModality{
		{contract.RealtimeOutputAudio}, {contract.RealtimeOutputText}, {contract.RealtimeOutputText, contract.RealtimeOutputAudio},
	}
	for _, d := range []*dialect{openAIDialect, xAIDialect} {
		parameters := d.statement().Parameters
		check := func(name string, config contract.RealtimeSessionConfig) {
			t.Helper()
			issues := parameters.Unservable("config", config.Fields())
			_, err := d.wireSession(config)
			var unsupported provider.ErrUnsupported
			switch {
			case len(issues) == 0 && err != nil:
				t.Errorf("%s %s: servable and refused: %v", d.slug, name, err)
			case len(issues) > 0 && (!errors.As(err, &unsupported) || unsupported.Param != issues[0].Param):
				t.Errorf("%s %s: unservable (%s); wireSession = %v", d.slug, name, issues[0].Param, err)
			case len(issues) > 0 && strings.Contains(unsupported.Detail, d.name):
				t.Errorf("%s %s: the refusal names the provider: %q", d.slug, name, unsupported.Detail)
			}
		}
		for _, detection := range detections {
			for _, choice := range choices {
				for _, set := range modalities {
					for _, transcription := range []*contract.RealtimeInputTranscription{
						nil, {Language: pointer("es-AR"), Prompt: pointer("names: Kaana")}, {Language: pointer("gl")},
					} {
						config := conversationConfig()
						config.TurnDetection, config.ToolChoice, config.OutputModalities = detection, &choice, set
						config.InputAudioTranscription, config.MaxOutputTokens = transcription, pointer(8192)
						check(fmt.Sprintf("%v/%v/%v/%v", detection.Type, contract.RealtimeToolChoiceOf(choice), set, transcription != nil), config)
					}
				}
			}
		}
	}
}

// TestXAITranslatesWhatItsWireSpellsDifferently pins the translations onto
// xAI's wire: semantic_vad runs as its server_vad, a detection that names no
// silence waits Kaana's default and one that does waits its own, the
// contract's threshold is mapped onto xAI's 0.1-0.9, `none` withholds the
// tools, a cap past the wire's is the wire's, and a prompt (a hint) is not
// sent where xAI has none.
func TestXAITranslatesWhatItsWireSpellsDifferently(t *testing.T) {
	yes := true
	config := pushToTalk()
	config.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionSemanticVAD, Eagerness: pointer(contract.RealtimeVADEagerness("high")), CreateResponse: &yes, InterruptResponse: &yes}
	config.ToolChoice = &contract.ToolChoice{Mode: pointer(contract.ToolChoiceNone)}
	config.InputAudioTranscription = &contract.RealtimeInputTranscription{Language: pointer("pt-AO"), Prompt: pointer("names")}
	wire, err := xAIDialect.wireSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(t, wire.TurnDetection); got != `{"silence_duration_ms":500,"type":"server_vad"}` {
		t.Errorf("semantic_vad on xAI = %s", got)
	}
	if wire.Tools == nil || len(*wire.Tools) != 0 || wire.ToolChoice != nil {
		t.Errorf("toolChoice none = tools %v, tool_choice %s; want the tools withheld", wire.Tools, wire.ToolChoice)
	}
	transcription := wire.Audio.Input.Transcription
	if transcription.Prompt != nil || transcription.LanguageHint == nil || *transcription.LanguageHint != "pt-BR" {
		t.Errorf("transcription = %+v; want pt-BR and no prompt", transcription)
	}
	config.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionServerVAD, Threshold: pointer(0.5), SilenceDurationMs: pointer(800), CreateResponse: &yes, InterruptResponse: &yes}
	if wire, err = xAIDialect.wireSession(config); err != nil || asJSON(t, wire.TurnDetection) != `{"silence_duration_ms":800,"threshold":0.5,"type":"server_vad"}` {
		t.Errorf("the contract's middle threshold on xAI = %s, %v; want xAI's middle", asJSON(t, wire.TurnDetection), err)
	}
	openAI := conversationConfig()
	openAI.MaxOutputTokens = pointer(9000)
	if wire, err = openAIDialect.wireSession(openAI); err != nil || wire.MaxOutputTokens == nil || *wire.MaxOutputTokens != 4096 {
		t.Errorf("a cap past OpenAI's = %v, %v; want 4096", wire.MaxOutputTokens, err)
	}
}

// TestEveryCommittedItemGetsOneTranscript: an item whose final transcript
// never comes is given the one it has when the next item is committed, before
// that commit, and a late transcript of it afterwards relays nothing more.
func TestEveryCommittedItemGetsOneTranscript(t *testing.T) {
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.updated", "item_id": "item_1", "transcript": "uno"})
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_1"})
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_2", "previous_item_id": "item_1"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_1", "transcript": "uno dos"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_2", "transcript": "tres"})
		conn.Send(fake.Event{"type": "input_audio_buffer.cleared"})
	}
	session := xaiOpen(t, upstream, transcribed(pushToTalk(), contract.RealtimeInputTranscription{}))
	events := collectUntil(t, session, contract.RealtimeEventInputAudioClearedType)
	want := []contract.RealtimeEventType{
		contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventInputAudioCommittedType,
		contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventInputAudioCommittedType,
		contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventTranscriptDoneType,
		contract.RealtimeEventInputAudioClearedType,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	first, second := events[2].Event.(*contract.RealtimeTranscriptDoneEvent), events[5].Event.(*contract.RealtimeTranscriptDoneEvent)
	if first.ItemID != "item_1" || first.Transcript != "uno" || second.ItemID != "item_2" || second.Transcript != "tres" {
		t.Errorf("transcripts = %+v, %+v", first, second)
	}
}
