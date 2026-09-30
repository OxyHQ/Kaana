package openairealtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

func xaiRoute() provider.Route {
	return provider.Route{
		DeploymentID: "dep_xai_voice", Provider: XAISlug,
		ModelReference: "x-ai/grok-voice-think-fast-2.0@observed-2026-09-30", UpstreamModelID: "grok-voice-think-fast-2.0",
	}
}

// pushToTalk is the one turn detection Kaana serves on xAI: the client commits
// its own turns, and xAI bills only the audio sent and received.
func pushToTalk() contract.RealtimeSessionConfig {
	return contract.RealtimeSessionConfig{
		Instructions:      pointer("be brief"),
		OutputModalities:  []contract.RealtimeOutputModality{contract.RealtimeOutputAudio},
		Voice:             pointer("eve"),
		InputAudioFormat:  contract.RealtimePCM16,
		OutputAudioFormat: pointer(contract.RealtimeULaw),
		TurnDetection:     contract.RealtimeTurnDetection{Type: contract.TurnDetectionNone},
		Tools: []contract.ToolDefinition{{
			Type: "function", Name: "lookup", Description: pointer("find an order"),
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
		}},
	}
}

func xaiAdapter(t *testing.T, upstream *fake.Upstream) *Adapter {
	t.Helper()
	adapter, err := NewXAI(Config{Declarations: []provider.KeyDeclaration{{KeyID: "key_xai_voice", Secret: fake.XAIKey}}, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func xaiOpen(t *testing.T, upstream *fake.Upstream, config contract.RealtimeSessionConfig) provider.RealtimeUpstream {
	t.Helper()
	request := provider.RealtimeOpenRequest{RequestID: requestID, Route: xaiRoute(), Kind: contract.RealtimeConversation, Config: config}
	session, opened, err := xaiAdapter(t, upstream).Open(context.Background(), request, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.KeyID != "key_xai_voice" {
		t.Fatalf("the open attempt names key %q", opened.KeyID)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestXAIOpenConfiguresTheSessionOnXAIsWire pins the configuring
// session.update against xAI's documented shape: `voice` and `turn_detection`
// on the session itself, push-to-talk spelled `{"type": null}`, the flat
// function tool its guide uses, and no OpenAI-only field (`type`,
// `output_modalities`, `tool_choice`, `max_output_tokens`). The session's
// output modalities have no session field on xAI; they ride on each
// response.create (TestXAICommandsItCannotExpressAreRefusedUnsent).
func TestXAIOpenConfiguresTheSessionOnXAIsWire(t *testing.T) {
	upstream := fake.NewXAI(t)
	xaiOpen(t, upstream, pushToTalk())
	received := upstream.Received()
	if len(received) != 1 {
		t.Fatalf("the fake read %d client events, want exactly the configuring session.update", len(received))
	}
	want := `{"event_id":"kaana-session-open","session":{"audio":{"input":{"format":{"rate":24000,"type":"audio/pcm"}},"output":{"format":{"type":"audio/pcmu"}}},"instructions":"be brief","tools":[{"description":"find an order","name":"lookup","parameters":{"properties":{"id":{"type":"string"}},"type":"object"},"type":"function"}],"turn_detection":{"type":null},"voice":"eve"},"type":"session.update"}`
	if got := asJSON(t, received[0]); got != want {
		t.Errorf("session.update =\n%s\nwant\n%s", got, want)
	}
	if models := upstream.Models(); !reflect.DeepEqual(models, []string{"grok-voice-think-fast-2.0"}) {
		t.Errorf("the handshake named models %v; it must name the signed, pinned upstream model", models)
	}
}

// TestWhatXAICannotExpressIsRefusedBeforeAnythingIsDialled: every refusal
// class spends nothing — not one handshake reaches xAI — and the unmodified
// push-to-talk request opening on the same fake is the control.
func TestWhatXAICannotExpressIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	cases := map[string]struct {
		mutate func(*provider.RealtimeOpenRequest)
		code   contract.ErrorCode
		param  string
	}{
		"server vad is billed by session duration": {func(r *provider.RealtimeOpenRequest) {
			r.Config.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionServerVAD, CreateResponse: pointer(true), InterruptResponse: pointer(true)}
		}, contract.CodeInvalidRequest, "config.turnDetection"},
		"semantic vad": {func(r *provider.RealtimeOpenRequest) {
			r.Config.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionSemanticVAD, Eagerness: pointer(contract.RealtimeVADEagerness("low")), CreateResponse: pointer(true), InterruptResponse: pointer(true)}
		}, contract.CodeInvalidRequest, "config.turnDetection"},
		"tool choice": {func(r *provider.RealtimeOpenRequest) {
			r.Config.ToolChoice = &contract.ToolChoice{Mode: pointer(contract.ToolChoiceAuto)}
		}, contract.CodeInvalidRequest, "config.toolChoice"},
		"output token limit": {func(r *provider.RealtimeOpenRequest) { r.Config.MaxOutputTokens = pointer(100) }, contract.CodeInvalidRequest, "config.maxOutputTokens"},
		"temperature":        {func(r *provider.RealtimeOpenRequest) { r.Config.Temperature = pointer(0.5) }, contract.CodeInvalidRequest, "config.temperature"},
		"input transcription": {func(r *provider.RealtimeOpenRequest) {
			r.Config.InputAudioTranscription = &contract.RealtimeInputTranscription{Language: pointer("en")}
		}, contract.CodeInvalidRequest, "config.inputAudioTranscription"},
		"transcription kind": {func(r *provider.RealtimeOpenRequest) { r.Kind = contract.RealtimeTranscription }, contract.CodeUnsupportedModality, "kind"},
		"another slug":       {func(r *provider.RealtimeOpenRequest) { r.Route.Provider = "xai" }, contract.CodeInvalidRequest, "authorizedRoutes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := fake.NewXAI(t)
			request := provider.RealtimeOpenRequest{RequestID: requestID, Route: xaiRoute(), Kind: contract.RealtimeConversation, Config: pushToTalk()}
			tc.mutate(&request)
			_, _, err := xaiAdapter(t, upstream).Open(context.Background(), request, nil)
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Code != tc.code || unsupported.Param != tc.param {
				t.Fatalf("Open = %v; want %s naming %s", err, tc.code, tc.param)
			}
			if upstream.Dials() != 0 {
				t.Errorf("a refused session dialled xAI %d times", upstream.Dials())
			}
		})
	}
	upstream := fake.NewXAI(t)
	xaiOpen(t, upstream, pushToTalk())
	if upstream.Dials() != 1 {
		t.Fatalf("the control dialled %d times", upstream.Dials())
	}
}

// TestXAIMetersWhatXAIBills drives one push-to-talk turn through xAI's wire
// and checks the session's measurement is exactly xAI's bill: audio sent and
// received by duration at each direction's byte rate, one fee per billed text
// item, nothing for a tool result, an audio item or response.create, and none
// of the token counts xAI reports but does not bill.
func TestXAIMetersWhatXAIBills(t *testing.T) {
	appended := bytes.Repeat([]byte{7}, 4800) // 100 ms of 24 kHz PCM16
	itemAudio := bytes.Repeat([]byte{8}, 480) // 10 ms more
	spoken := bytes.Repeat([]byte{9}, 800)    // 100 ms of 8 kHz G.711 u-law
	tail := []byte{1, 2, 3, 4, 5, 6, 7, 8}    // 1 ms more, under the legacy name
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		for _, want := range []string{
			"input_audio_buffer.append", "input_audio_buffer.append", "input_audio_buffer.commit",
			"conversation.item.create", "conversation.item.create", "conversation.item.create", "response.create",
		} {
			conn.Expect(want)
		}
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "conversation.item.added", "item": fake.Event{"id": "item_answer", "object": "realtime.item", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{fake.Event{"type": "text", "text": "Hi"}, fake.Event{"type": "audio", "transcript": "Hi"}}}})
		conn.Send(fake.Event{"type": "response.text.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0, "delta": "Hi"})
		conn.Send(fake.Event{"type": "response.output_audio_transcript.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 1, "delta": "Hi"})
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 1, "delta": base64.StdEncoding.EncodeToString(spoken)})
		conn.Send(fake.Event{"type": "response.audio.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 1, "delta": base64.StdEncoding.EncodeToString(tail)})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "completed", "usage": fake.Event{"input_tokens": 900, "output_tokens": 400, "total_tokens": 1300}}})
	}
	session := xaiOpen(t, upstream, pushToTalk())
	half := base64.StdEncoding.EncodeToString(appended[:2400])
	send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_a1", Data: half})
	send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_a2", Data: half})
	send(t, session, &contract.RealtimeInputAudioCommitCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_commit"})
	user, text := contract.RealtimeItemRole("user"), "and the order?"
	send(t, session, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_text", Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputTextPart, Text: &text}}}})
	callID, output := "call_1", `{"status":"shipped"}`
	send(t, session, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_tool", Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeFunctionCallOutputItem, CallID: &callID, Output: &output}})
	encoded, format := base64.StdEncoding.EncodeToString(itemAudio), contract.RealtimePCM16
	send(t, session, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_audio_item", Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputAudioPart, Data: &encoded, Format: &format}}}})
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})

	var events []provider.RealtimeUpstreamEvent
	for len(events) == 0 || events[len(events)-1].Event.EventType() != contract.RealtimeEventResponseDoneType {
		events = append(events, next(t, session))
	}
	want := []contract.RealtimeEventType{
		contract.RealtimeEventResponseCreatedType, contract.RealtimeEventItemAddedType, contract.RealtimeEventTextDeltaType,
		contract.RealtimeEventTranscriptDeltaType, contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventOutputAudioDeltaType,
		contract.RealtimeEventResponseDoneType,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	added := events[1].Event.(*contract.RealtimeItemAddedEvent)
	if len(added.Item.Content) != 2 || added.Item.Content[0].Type != contract.RealtimeOutputTextPart || added.Item.Content[1].Type != contract.RealtimeOutputAudioPart ||
		*added.Item.Content[1].Format != contract.RealtimeULaw {
		t.Errorf("xAI's assistant parts were not read as the contract's: %+v", added.Item.Content)
	}
	done := events[6]
	response := done.Event.(*contract.RealtimeResponseDoneEvent)
	if len(response.Units) != 0 || len(done.Units) != 0 || response.UsageSource != contract.UsageOxyMeasured {
		t.Errorf("response.done settled xAI's unbilled tokens: %+v, %v", response, done.Units)
	}

	meter, metered := session.(provider.RealtimeMeter)
	if !metered {
		t.Fatalf("the xAI session %T is not metered", session)
	}
	wantUnits := []contract.UsageQuantity{
		{Unit: contract.UnitAudioInputMilliseconds, Quantity: 110},
		{Unit: contract.UnitAudioOutputMilliseconds, Quantity: 101},
		{Unit: contract.UnitRequests, Quantity: 1},
	}
	if got := meter.Measured(); !reflect.DeepEqual(got, wantUnits) {
		t.Errorf("measured %+v, want %+v", got, wantUnits)
	}

	// The legacy output name and the audio item's input must each have been
	// counted exactly once: a second read changes nothing.
	if got := meter.Measured(); !reflect.DeepEqual(got, wantUnits) {
		t.Errorf("a second read measured %+v", got)
	}
}

// TestAnOpenAISessionIsNotMetered is the control for the metering: OpenAI
// bills the tokens it reports, so its session measures nothing of its own.
func TestAnOpenAISessionIsNotMetered(t *testing.T) {
	session := open(t, fake.New(t), conversationConfig())
	if _, metered := session.(provider.RealtimeMeter); metered {
		t.Fatalf("the OpenAI session %T is metered", session)
	}
}

// TestXAICommandsItCannotExpressAreRefusedUnsent: a command xAI cannot express,
// or whose billing it does not document, is refused before a byte is written
// and measures nothing; response.create carries xAI's `modalities`.
func TestXAICommandsItCannotExpressAreRefusedUnsent(t *testing.T) {
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		created := conn.Expect("response.create")
		if got := asJSON(t, created["response"]); got != `{"instructions":"short","modalities":["text","audio"]}` {
			t.Errorf("response.create = %s", got)
		}
		// A bare response.create carries the session's modalities, which
		// xAI has no session field for.
		bare := conn.Expect("response.create")
		if got := asJSON(t, bare["response"]); got != `{"modalities":["audio"]}` {
			t.Errorf("bare response.create = %s", got)
		}
	}
	session := xaiOpen(t, upstream, pushToTalk())
	user, text, audio := contract.RealtimeItemRole("user"), "hi", base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})
	format := contract.RealtimePCM16
	for name, command := range map[string]contract.RealtimeCommand{
		"server vad update": &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_vad", Config: contract.RealtimeSessionConfigUpdate{
			TurnDetection: &contract.RealtimeTurnDetection{Type: contract.TurnDetectionServerVAD, CreateResponse: pointer(true), InterruptResponse: pointer(true)}}},
		"text and audio in one item": &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_mixed", Item: contract.RealtimeConversationItem{
			Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputTextPart, Text: &text}, {Type: contract.RealtimeInputAudioPart, Data: &audio, Format: &format}}}},
		"response token limit": &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_limit", Response: &contract.RealtimeResponseParameters{MaxOutputTokens: pointer(10)}},
	} {
		var unsupported provider.ErrUnsupported
		if err := session.Send(context.Background(), command); !errors.As(err, &unsupported) {
			t.Errorf("%s: Send = %v; want a refusal before anything is written", name, err)
		}
	}
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond", Response: &contract.RealtimeResponseParameters{
		Instructions: pointer("short"), OutputModalities: []contract.RealtimeOutputModality{contract.RealtimeOutputText, contract.RealtimeOutputAudio}}})
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_bare"})
	_ = session.Close()
	upstream.WaitClosed(t)
	if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.update", "response.create", "response.create"}) {
		t.Errorf("xAI received %v; a refused command reached it", types)
	}
	if got := session.(provider.RealtimeMeter).Measured(); len(got) != 0 {
		t.Errorf("refused commands were measured: %+v", got)
	}
}

// TestXAIErrorsAreClassifiedByXAIsTypes: an in-band refusal of one event keeps
// the session open and names the command; a failure of the session itself is
// what the connection then ends with; the key never reaches the customer.
func TestXAIErrorsAreClassifiedByXAIsTypes(t *testing.T) {
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.commit")
		conn.Send(fake.Event{"type": "error", "error": fake.Event{"type": "invalid_request_error", "code": "invalid_audio_format", "message": "bad audio for " + fake.XAIKey, "event_id": "cmd_commit"}})
		conn.Send(fake.Event{"type": "error", "error": fake.Event{"type": "max_duration", "code": "max_duration", "message": "session exceeded 120 minutes"}})
		conn.Drop()
	}
	session := xaiOpen(t, upstream, pushToTalk())
	send(t, session, &contract.RealtimeInputAudioCommitCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_commit"})

	refusal := next(t, session)
	event := refusal.Event.(*contract.RealtimeErrorEvent)
	if event.Fatal || event.Error.Code != contract.CodeInvalidRequest || refusal.CommandID == nil || *refusal.CommandID != "cmd_commit" {
		t.Errorf("refusal = %+v (%v)", event, refusal.CommandID)
	}
	if strings.Contains(asJSON(t, event), fake.XAIKey) {
		t.Error("xAI's echo of the key reached the customer")
	}
	ended := next(t, session).Event.(*contract.RealtimeErrorEvent)
	if ended.Error.Code != contract.CodeProviderTimeout {
		t.Errorf("max_duration = %+v", ended.Error)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := session.Next(ctx)
	var upstreamErr provider.ErrUpstream
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != contract.CodeProviderTimeout {
		t.Errorf("the dropped session ended with %v; want the max_duration failure xAI reported", err)
	}
}

// TestXAIHandshakeRefusals: 401 is xAI refusing the credential (the key is
// retired); the 400 xAI answered an invalid key with when probed labels itself
// an invalid argument and is read as one, its message kept and redacted.
func TestXAIHandshakeRefusals(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		code   contract.ErrorCode
	}{
		{http.StatusUnauthorized, `{"code":"The request does not have valid authentication credentials","error":"No credentials presented."}`, contract.CodeProviderCredentialInvalid},
		{http.StatusBadRequest, `{"code":"Client specified an invalid argument","error":"Incorrect API key provided: ` + fake.XAIKey + `"}`, contract.CodeInvalidRequest},
		{http.StatusTooManyRequests, `{"code":"Too many requests","error":"You are sending requests too frequently"}`, contract.CodeRateLimited},
	} {
		upstream := fake.NewXAI(t)
		upstream.Refuse = func(w http.ResponseWriter, _ *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
			return true
		}
		request := provider.RealtimeOpenRequest{RequestID: requestID, Route: xaiRoute(), Kind: contract.RealtimeConversation, Config: pushToTalk()}
		_, _, err := xaiAdapter(t, upstream).Open(context.Background(), request, nil)
		var upstreamErr provider.ErrUpstream
		if !errors.As(err, &upstreamErr) || upstreamErr.Code != c.code {
			t.Errorf("%d: Open = %v; want %s", c.status, err, c.code)
			continue
		}
		if upstreamErr.Passthrough == nil || upstreamErr.Passthrough.Provider != XAISlug || upstreamErr.Passthrough.Message == nil ||
			strings.Contains(*upstreamErr.Passthrough.Message, fake.XAIKey) {
			t.Errorf("%d: passthrough = %+v", c.status, upstreamErr.Passthrough)
		}
	}
}
