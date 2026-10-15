package openairealtime

import (
	"context"
	"encoding/base64"
	"errors"
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
	session := open(t, upstream, transcribed(conversationConfig(), contract.RealtimeInputTranscription{Language: pointer("ES"), Prompt: pointer("names: Kaana, Alia")}))

	update := upstream.Received()[0]
	wantTranscription := map[string]any{"model": "gpt-transcribe", "language": "es", "prompt": "names: Kaana, Alia"}
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
		if want := map[string]any{"model": "gpt-transcribe", "language": "en"}; !reflect.DeepEqual(got, want) {
			t.Errorf("the enabling update sent %v, want %v", got, want)
		}
		conn.Send(fake.Event{"type": "session.updated", "session": update["session"]})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_3", "content_index": 0, "transcript": "now", "usage": fake.TranscriptionDuration(0.25)})
	}
	session := open(t, upstream, conversationConfig())
	silent := next(t, session)
	if silent.Event != nil || !reflect.DeepEqual(silent.Units, []contract.UsageQuantity{{Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: 500}}) {
		t.Fatalf("an unrequested transcription = %+v; want only its charge", silent)
	}
	send(t, session, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_transcribe",
		Config: contract.RealtimeSessionConfigUpdate{InputAudioTranscription: &contract.RealtimeInputTranscription{Language: pointer("en")}}})
	confirmed := next(t, session)
	updated := confirmed.Event.(*contract.RealtimeSessionUpdatedEvent)
	if updated.Config.InputAudioTranscription == nil || *updated.Config.InputAudioTranscription.Language != "en" {
		t.Errorf("the effective configuration lost the transcription: %+v", updated.Config)
	}
	relayed := next(t, session)
	done, ok := relayed.Event.(*contract.RealtimeTranscriptDoneEvent)
	if !ok || done.ItemID != "item_3" || !reflect.DeepEqual(relayed.Units, []contract.UsageQuantity{{Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: 250}}) {
		t.Errorf("after enabling, the transcript = %+v", relayed)
	}
}

// TestXAITranscribesInsideItsSessionPrice: xAI's wire names `grok-transcribe`
// and a `language_hint` from its documented list, its completed event (no
// content_index, no usage) becomes the caller's transcript, its cumulative
// `updated` events are not relayed, and the session's measurement is exactly
// what it would be without transcription: xAI's pricing has no transcription
// line.
func TestXAITranscribesInsideItsSessionPrice(t *testing.T) {
	appended := base64.StdEncoding.EncodeToString(make([]byte, 4800)) // 100 ms of PCM16
	script := func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.updated", "item_id": "item_user", "transcript": "Hol"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "transcript": "Hola"})
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

	events, measured, upstream := drive(transcribed(pushToTalk(), contract.RealtimeInputTranscription{Language: pointer("es-MX")}))
	update := sessionOf(t, upstream.Received()[0])
	got := update["audio"].(map[string]any)["input"].(map[string]any)["transcription"]
	if want := map[string]any{"model": "grok-transcribe", "language_hint": "es-MX"}; !reflect.DeepEqual(got, want) {
		t.Errorf("audio.input.transcription = %v, want %v", got, want)
	}
	if update["turn_detection"] == nil || update["voice"] != "eve" {
		t.Errorf("xAI's flat session fields were displaced: %v", update)
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, []contract.RealtimeEventType{contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventInputAudioClearedType}) {
		t.Fatalf("events = %v", got)
	}
	done := events[0].Event.(*contract.RealtimeTranscriptDoneEvent)
	if done.Source != contract.RealtimeTranscriptInput || done.ItemID != "item_user" || done.ContentIndex != 0 || done.Transcript != "Hola" || len(events[0].Units) != 0 {
		t.Errorf("xAI's input transcript = %+v units %v", done, events[0].Units)
	}

	// Control: the same session without transcription relays no transcript
	// and is measured identically.
	plainEvents, plainMeasured, _ := drive(pushToTalk())
	if got := eventTypes(plainEvents); !reflect.DeepEqual(got, []contract.RealtimeEventType{contract.RealtimeEventInputAudioClearedType}) {
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

// TestTheCatalogueStatementIsWhatTheAdapterServes ties the published
// statement (providerconfig.RealtimeCapabilities) to this adapter: the flag
// is set exactly where a dialect transcribes, every published encoding is one
// the wire translates, and every language xAI publishes is one its
// transcription takes — while one it does not publish is refused.
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
		for _, language := range statement.Languages {
			if _, ok := d.transcription.language(language); !ok {
				t.Errorf("%s publishes %s, which its transcription refuses", d.slug, language)
			}
		}
	}
	if _, ok := xAITranscription.language("es"); ok {
		t.Error("xAI's transcription took a Spanish tag without the region xAI requires")
	}
	if language, ok := openAITranscription.language("EN"); !ok || language != "en" {
		t.Errorf("OpenAI's transcription = %q, %t; want the lowercased ISO 639-1 code", language, ok)
	}
}
