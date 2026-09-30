package openairealtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// contextWait bounds a Next that is expected to fail.
const contextWait = 10 * time.Second

// captured is every server event of testdata/xai-text-only-capture.jsonl:
// what xAI's production Voice Agent API sent on 2026-09-30 for a session
// configured `modalities: ["text"]`, `turn_detection: {"type": null}`, one
// input_text item and a bare response.create. Each line is `<type> <json>`;
// the capture elided each audio delta's base64, which the replay refills with
// exactly the `audio_duration_ms` xAI stated for it at 24 kHz PCM16.
func captured(t *testing.T) []fake.Event {
	t.Helper()
	data, err := os.ReadFile("testdata/xai-text-only-capture.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var events []fake.Event
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		kind, encoded, found := strings.Cut(scanner.Text(), " ")
		if !found {
			t.Fatalf("capture line %q has no event", scanner.Text())
		}
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		var event fake.Event
		if err := decoder.Decode(&event); err != nil || event.Type() != kind {
			t.Fatalf("capture line %q: %v", kind, err)
		}
		if event["delta"] == "<base64 audio elided>" {
			milliseconds, err := event["audio_duration_ms"].(json.Number).Int64()
			if err != nil {
				t.Fatal(err)
			}
			event["delta"] = base64.StdEncoding.EncodeToString(make([]byte, milliseconds*48))
		}
		events = append(events, event)
	}
	return events
}

// replayXAI serves the capture: xAI's own opening events, xAI's own
// session.updated as the confirmation, and — once the adapter has written the
// item and the response.create — every event xAI sent after them, verbatim.
func replayXAI(t *testing.T, events []fake.Event, then func(conn *fake.Conn)) *fake.Upstream {
	t.Helper()
	upstream := fake.NewXAI(t)
	updated := -1
	for index, event := range events {
		if event.Type() == "session.updated" {
			updated = index
		}
	}
	if updated < 0 {
		t.Fatal("the capture holds no session.updated")
	}
	upstream.Greeting, upstream.Confirmation = events[:updated], events[updated]
	upstream.Script = func(conn *fake.Conn) {
		conn.Expect("conversation.item.create")
		created := conn.Expect("response.create")
		if got := asJSON(t, created["response"]); got != `{"modalities":["audio"]}` {
			t.Errorf("response.create = %s", got)
		}
		for _, event := range events[updated+1:] {
			conn.Send(event)
		}
		if then != nil {
			then(conn)
		}
	}
	return upstream
}

// spokenPushToTalk is the capture's session as Kaana serves it on xAI: push-
// to-talk, spoken (xAI speaks whatever it is asked, below), in 24 kHz PCM16 so
// the capture's stated durations are its byte counts.
func spokenPushToTalk() contract.RealtimeSessionConfig {
	return contract.RealtimeSessionConfig{
		Instructions:      pointer("Responde en una sola palabra."),
		OutputModalities:  []contract.RealtimeOutputModality{contract.RealtimeOutputAudio},
		Voice:             pointer("ara"),
		InputAudioFormat:  contract.RealtimePCM16,
		OutputAudioFormat: pointer(contract.RealtimePCM16),
		TurnDetection:     contract.RealtimeTurnDetection{Type: contract.TurnDetectionNone},
	}
}

func askForHola(t *testing.T, session provider.RealtimeUpstream) {
	t.Helper()
	user, text := contract.RealtimeItemRole("user"), "Di hola."
	send(t, session, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_item", Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputTextPart, Text: &text}}}})
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})
}

// TestXAICapturedResponseIsReadAndMetered replays the production capture of
// the flow that failed on 2026-09-30 (requestId 253dacf6-...: "xAI sent a
// Realtime event this adapter cannot read" right after response.create). The
// whole response is read — xAI's opening `ping`, its `session.updated` with
// `turn_detection: {}`, `temperature: -1`, `max_response_output_tokens:
// "inf"`; `status_details: "unimplemented"` and `usage: {}` on
// response.created and response.done; the `audio` content part with
// `previous_item_id: "0"`; the assistant item added with no content — and the
// session relays the spoken answer's transcript and meters its audio: 710 ms,
// exactly the `output_audio_seconds: 0.71` xAI reported for it.
func TestXAICapturedResponseIsReadAndMetered(t *testing.T) {
	upstream := replayXAI(t, captured(t), nil)
	session := xaiOpen(t, upstream, spokenPushToTalk())
	askForHola(t, session)

	var events []provider.RealtimeUpstreamEvent
	for len(events) == 0 || events[len(events)-1].Event.EventType() != contract.RealtimeEventResponseDoneType {
		events = append(events, next(t, session))
	}
	want := []contract.RealtimeEventType{
		contract.RealtimeEventItemAddedType, contract.RealtimeEventResponseCreatedType,
		contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventTranscriptDeltaType,
		contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventOutputAudioDeltaType,
		contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventOutputAudioDoneType,
		contract.RealtimeEventResponseDoneType,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	if added := events[0].Event.(*contract.RealtimeItemAddedEvent); *added.Item.Role != "user" || *added.Item.Content[0].Text != "Di hola." {
		t.Errorf("the user's item = %+v", added.Item)
	}
	if created := events[1].Event.(*contract.RealtimeResponseCreatedEvent); created.ResponseID != "7cc696c6-57a4-4636-bb7e-4095168258b8" {
		t.Errorf("response.created = %+v", created)
	}
	if delta := events[3].Event.(*contract.RealtimeTranscriptDeltaEvent); delta.Text != "Hola" || delta.Source != contract.RealtimeTranscriptOutput {
		t.Errorf("transcript delta = %+v", delta)
	}
	if done := events[6].Event.(*contract.RealtimeTranscriptDoneEvent); done.Transcript != "Hola" {
		t.Errorf("transcript done = %+v", done)
	}
	relayed := 0
	for _, event := range events {
		if audio, ok := event.Event.(*contract.RealtimeOutputAudioDeltaEvent); ok {
			decoded, _ := base64.StdEncoding.DecodeString(audio.Data)
			relayed += len(decoded)
		}
	}
	if relayed != 710*48 {
		t.Errorf("relayed %d bytes of audio, want the capture's 710 ms", relayed)
	}
	done := events[8].Event.(*contract.RealtimeResponseDoneEvent)
	if done.Status != contract.RealtimeResponseCompleted || done.FinishReason == nil || *done.FinishReason != contract.FinishStop ||
		len(done.Units) != 0 || done.UsageSource != contract.UsageOxyMeasured {
		t.Errorf("response.done = %+v", done)
	}

	wantUnits := []contract.UsageQuantity{
		{Unit: contract.UnitAudioOutputMilliseconds, Quantity: 710},
		{Unit: contract.UnitRequests, Quantity: 1},
	}
	if got := session.(provider.RealtimeMeter).Measured(); !reflect.DeepEqual(got, wantUnits) {
		t.Errorf("measured %+v, want %+v", got, wantUnits)
	}
}

// TestAStringStatusDetailsIsXAIsAlone: xAI's string status_details is read on
// xAI only. OpenAI documents an object or null, and a string there is still an
// event the adapter cannot read, named by its type and field; so is any other
// kind on xAI, which is tolerated a string and nothing else. The null
// status_details on OpenAI is the control.
func TestAStringStatusDetailsIsXAIsAlone(t *testing.T) {
	respond := func(t *testing.T, upstream *fake.Upstream, session provider.RealtimeUpstream) error {
		t.Helper()
		send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})
		ctx, cancel := context.WithTimeout(context.Background(), contextWait)
		defer cancel()
		_, err := session.Next(ctx)
		return err
	}
	createdWith := func(details any) func(conn *fake.Conn) {
		return func(conn *fake.Conn) {
			conn.Expect("response.create")
			conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "in_progress", "status_details": details, "output": []any{}}})
		}
	}
	for name, c := range map[string]struct {
		openAI  bool
		details any
		detail  string
	}{
		"a string on OpenAI": {true, "unimplemented", "OpenAI sent a Realtime event this adapter cannot read (response.created: response.status_details is a JSON string)"},
		"a number on xAI":    {false, 7, "xAI sent a Realtime event this adapter cannot read (response.created: response.status_details is a JSON number)"},
		"an array on xAI":    {false, []any{"unimplemented"}, "xAI sent a Realtime event this adapter cannot read (response.created: response.status_details is a JSON array)"},
		"null on OpenAI":     {true, nil, ""},
		"a string on xAI":    {false, "unimplemented", ""},
	} {
		t.Run(name, func(t *testing.T) {
			var session provider.RealtimeUpstream
			var upstream *fake.Upstream
			if c.openAI {
				upstream = fake.New(t)
				upstream.Script = createdWith(c.details)
				session = open(t, upstream, conversationConfig())
			} else {
				upstream = fake.NewXAI(t)
				upstream.Script = createdWith(c.details)
				session = xaiOpen(t, upstream, pushToTalk())
			}
			err := respond(t, upstream, session)
			if c.detail == "" {
				if err != nil {
					t.Fatalf("Next = %v; want response.created", err)
				}
				return
			}
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) || failure.Code != contract.CodeProviderError || failure.Detail != c.detail {
				t.Fatalf("Next = %#v\nwant the detail %q", err, c.detail)
			}
		})
	}
}

// TestAnUnreadableEventIsNamed: the failure names the event's type and the
// field that did not fit, in this adapter's vocabulary, never a value the
// provider sent; a type that does not look like one is not echoed.
func TestAnUnreadableEventIsNamed(t *testing.T) {
	for name, c := range map[string]struct {
		frame  string
		detail string
	}{
		"a field of the wrong kind": {`{"type":"response.output_audio.delta","delta":12345}`,
			"(response.output_audio.delta: delta is a JSON number)"},
		// encoding/json names the literal of a number that does not fit
		// ("number 4.25"); only its kind is kept.
		"a number that does not fit": {`{"type":"response.output_audio.delta","content_index":4.25}`,
			"(response.output_audio.delta: content_index is a JSON number)"},
		"a nested field": {`{"type":"conversation.item.added","item":{"id":"item_1","type":"message","content":"secret words"}}`,
			"(conversation.item.added: item.content is a JSON string)"},
		"not JSON": {`not json at all`, "(not a JSON object)"},
		"a type that is not a type": {`{"type":"Bearer xai-abc DROP","item":7}`,
			"(an unrecognisable type: item is a JSON number)"},
		"a field the event lacks": {`{"type":"response.done"}`, "(response.done: response.id is absent)"},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := fake.NewXAI(t)
			upstream.Script = func(conn *fake.Conn) { conn.SendRaw([]byte(c.frame)) }
			session := xaiOpen(t, upstream, pushToTalk())
			ctx, cancel := context.WithTimeout(context.Background(), contextWait)
			defer cancel()
			_, err := session.Next(ctx)
			var failure provider.ErrUpstream
			want := "xAI sent a Realtime event this adapter cannot read " + c.detail
			if !errors.As(err, &failure) || failure.Detail != want {
				t.Fatalf("Next = %v\nwant %q", err, want)
			}
			if strings.Contains(failure.Detail, "secret") || strings.Contains(failure.Detail, "12345") || strings.Contains(failure.Detail, "4.25") || strings.Contains(failure.Detail, "Bearer") {
				t.Errorf("the detail carries what the provider sent: %q", failure.Detail)
			}
		})
	}
}

// TestXAIRefusesATextOnlyResponse: xAI answers aloud whatever modalities it
// is sent (the capture: `modalities: ["text"]` confirmed, then audio streamed
// and reported billable), so a text-only session is refused at open before
// xAI is dialled, and a text-only session.update or response.create before a
// byte is written. A spoken session, and text beside audio, are the controls;
// OpenAI's text-only session is the control that nothing else changed.
func TestXAIRefusesATextOnlyResponse(t *testing.T) {
	text := []contract.RealtimeOutputModality{contract.RealtimeOutputText}
	textOnly := spokenPushToTalk()
	textOnly.OutputModalities, textOnly.Voice, textOnly.OutputAudioFormat = text, nil, nil
	upstream := fake.NewXAI(t)
	request := provider.RealtimeOpenRequest{RequestID: requestID, Route: xaiRoute(), Kind: contract.RealtimeConversation, Config: textOnly}
	_, _, err := xaiAdapter(t, upstream).Open(context.Background(), request, nil)
	var unsupported provider.ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Code != contract.CodeInvalidRequest || unsupported.Param != "config.outputModalities" {
		t.Fatalf("Open = %v; want invalid_request naming config.outputModalities", err)
	}
	if upstream.Dials() != 0 {
		t.Errorf("a refused session dialled xAI %d times", upstream.Dials())
	}

	upstream = fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		created := conn.Expect("response.create")
		if got := asJSON(t, created["response"]); got != `{"modalities":["text","audio"]}` {
			t.Errorf("response.create = %s", got)
		}
	}
	session := xaiOpen(t, upstream, spokenPushToTalk())
	for name, c := range map[string]struct {
		command contract.RealtimeCommand
		param   string
	}{
		"a text-only update": {&contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_update",
			Config: contract.RealtimeSessionConfigUpdate{OutputModalities: text}}, "config.outputModalities"},
		"a text-only response": {&contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_text",
			Response: &contract.RealtimeResponseParameters{OutputModalities: text}}, "response.outputModalities"},
	} {
		var refusal provider.ErrUnsupported
		if err := session.Send(context.Background(), c.command); !errors.As(err, &refusal) || refusal.Param != c.param {
			t.Errorf("%s: Send = %v; want a refusal naming %s", name, err, c.param)
		}
	}
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_both",
		Response: &contract.RealtimeResponseParameters{OutputModalities: []contract.RealtimeOutputModality{contract.RealtimeOutputText, contract.RealtimeOutputAudio}}})
	_ = session.Close()
	upstream.WaitClosed(t)
	if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.update", "response.create"}) {
		t.Errorf("xAI received %v; a refused command reached it", types)
	}

	openAIText := conversationConfig()
	openAIText.OutputModalities, openAIText.Voice, openAIText.OutputAudioFormat = text, nil, nil
	open(t, fake.New(t), openAIText)
}

// TestAnIncompleteResponseReadsItsReason: OpenAI's status_details.reason
// decides an incomplete response's finish reason; xAI's string
// status_details states none, so its incomplete response has none rather
// than an invented one.
func TestAnIncompleteResponseReadsItsReason(t *testing.T) {
	doneWith := func(details any) func(conn *fake.Conn) {
		return func(conn *fake.Conn) {
			conn.Expect("response.create")
			conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "incomplete", "status_details": details, "output": []any{}}})
		}
	}
	upstream := fake.New(t)
	upstream.Script = doneWith(fake.Event{"type": "incomplete", "reason": "max_output_tokens"})
	session := open(t, upstream, conversationConfig())
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})
	if done := next(t, session).Event.(*contract.RealtimeResponseDoneEvent); done.FinishReason == nil || *done.FinishReason != contract.FinishLength {
		t.Errorf("OpenAI's incomplete response = %+v; want length", done)
	}

	upstream = fake.NewXAI(t)
	upstream.Script = doneWith("unimplemented")
	session = xaiOpen(t, upstream, pushToTalk())
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})
	if done := next(t, session).Event.(*contract.RealtimeResponseDoneEvent); done.Status != contract.RealtimeResponseIncomplete || done.FinishReason != nil {
		t.Errorf("xAI's incomplete response = %+v; want no finish reason", done)
	}
}
