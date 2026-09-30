package openairealtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// pushToTalk is xAI's audio-billed mode: the client commits its own turns,
// and xAI bills only the audio sent and received.
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

// serverVAD is xAI's clock-billed mode: xAI detects the turns and bills the
// session for its whole duration.
func serverVAD() contract.RealtimeSessionConfig {
	config := pushToTalk()
	config.TurnDetection = contract.RealtimeTurnDetection{
		Type: contract.TurnDetectionServerVAD, Threshold: pointer(0.5), SilenceDurationMs: pointer(500),
		CreateResponse: pointer(true), InterruptResponse: pointer(true),
	}
	return config
}

// clock is a controllable wall clock for the session meter.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func xaiAdapter(t *testing.T, upstream *fake.Upstream) *Adapter {
	t.Helper()
	return xaiAdapterWithClock(t, upstream, nil)
}

func xaiAdapterWithClock(t *testing.T, upstream *fake.Upstream, now func() time.Time) *Adapter {
	t.Helper()
	adapter, err := NewXAI(Config{Declarations: []provider.KeyDeclaration{{KeyID: "key_xai_voice", Secret: fake.XAIKey}}, HTTPClient: upstream.Client(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func xaiOpen(t *testing.T, upstream *fake.Upstream, config contract.RealtimeSessionConfig) provider.RealtimeUpstream {
	t.Helper()
	return xaiOpenWithClock(t, upstream, config, nil)
}

func xaiOpenWithClock(t *testing.T, upstream *fake.Upstream, config contract.RealtimeSessionConfig, now func() time.Time) provider.RealtimeUpstream {
	t.Helper()
	request := provider.RealtimeOpenRequest{RequestID: requestID, Route: xaiRoute(), Kind: contract.RealtimeConversation, Config: config}
	session, opened, err := xaiAdapterWithClock(t, upstream, now).Open(context.Background(), request, nil)
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
		"server vad that does not create responses": {func(r *provider.RealtimeOpenRequest) {
			r.Config.TurnDetection = serverVAD().TurnDetection
			r.Config.TurnDetection.CreateResponse = pointer(false)
		}, contract.CodeInvalidRequest, "config.turnDetection.createResponse"},
		"server vad that cannot be interrupted": {func(r *provider.RealtimeOpenRequest) {
			r.Config.TurnDetection = serverVAD().TurnDetection
			r.Config.TurnDetection.InterruptResponse = pointer(false)
		}, contract.CodeInvalidRequest, "config.turnDetection.interruptResponse"},
		"server vad threshold outside xAI's range": {func(r *provider.RealtimeOpenRequest) {
			r.Config.TurnDetection = serverVAD().TurnDetection
			r.Config.TurnDetection.Threshold = pointer(0.95)
		}, contract.CodeInvalidRequest, "config.turnDetection.threshold"},
		"server vad silence past xAI's bound": {func(r *provider.RealtimeOpenRequest) {
			r.Config.TurnDetection = serverVAD().TurnDetection
			r.Config.TurnDetection.SilenceDurationMs = pointer(10_001)
		}, contract.CodeInvalidRequest, "config.turnDetection.silenceDurationMs"},
		"text-only server vad session": {func(r *provider.RealtimeOpenRequest) {
			r.Config = serverVAD()
			r.Config.OutputModalities = []contract.RealtimeOutputModality{contract.RealtimeOutputText}
			r.Config.Voice, r.Config.OutputAudioFormat = nil, nil
		}, contract.CodeInvalidRequest, "config.outputModalities"},
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
		"a switch to server vad, which xAI bills differently": &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_vad", Config: contract.RealtimeSessionConfigUpdate{
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

// TestXAIOpenConfiguresServerVADOnXAIsWire pins server_vad against xAI's
// session schema: the type and the tunables xAI documents, and neither of
// OpenAI's create_response / interrupt_response, which xAI has no field for.
func TestXAIOpenConfiguresServerVADOnXAIsWire(t *testing.T) {
	upstream := fake.NewXAI(t)
	xaiOpen(t, upstream, serverVAD())
	received := upstream.Received()
	if len(received) != 1 {
		t.Fatalf("the fake read %d client events, want exactly the configuring session.update", len(received))
	}
	session, _ := received[0]["session"].(map[string]any)
	if got := asJSON(t, session["turn_detection"]); got != `{"silence_duration_ms":500,"threshold":0.5,"type":"server_vad"}` {
		t.Errorf("turn_detection = %s", got)
	}
}

// TestXAIServerVADMetersTheSessionClock: a server_vad session is billed for
// its wall clock, from the accepted handshake to the upstream's close, plus
// its text items — and not for the audio it carried, which the clock already
// bills. The push-to-talk session on the same script is the control: the same
// audio is metered as audio there, and no clock is reported.
func TestXAIServerVADMetersTheSessionClock(t *testing.T) {
	spoken := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 800)) // 100 ms of G.711
	script := func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		conn.Expect("conversation.item.create")
		conn.Send(fake.Event{"type": "input_audio_buffer.speech_started", "item_id": "item_user", "audio_start_ms": 0})
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0, "delta": spoken})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "completed", "usage": fake.Event{"input_tokens": 900, "output_tokens": 400, "total_tokens": 1300}}})
	}
	drive := func(t *testing.T, config contract.RealtimeSessionConfig, clock *clock) provider.RealtimeMeter {
		t.Helper()
		upstream := fake.NewXAI(t)
		upstream.Script = script
		session := xaiOpenWithClock(t, upstream, config, clock.Now)
		clock.advance(20 * time.Second)
		send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_audio",
			Data: base64.StdEncoding.EncodeToString(make([]byte, 4800))}) // 100 ms of PCM16
		user, text := contract.RealtimeItemRole("user"), "and the order?"
		send(t, session, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_text", Item: contract.RealtimeConversationItem{
			Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputTextPart, Text: &text}}}})
		for next(t, session).Event.EventType() != contract.RealtimeEventResponseDoneType {
		}
		clock.advance(70*time.Second + 400*time.Microsecond)
		meter := session.(provider.RealtimeMeter)
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		upstream.WaitClosed(t)
		// Time after the close is not the session's.
		clock.advance(time.Hour)
		return meter
	}

	vad := drive(t, serverVAD(), newClock())
	// 90 000.4 ms from the handshake to the close, rounded up once.
	want := []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 90_001}, {Unit: contract.UnitRequests, Quantity: 1}}
	if got := vad.Measured(); !reflect.DeepEqual(got, want) {
		t.Errorf("server_vad measured %+v, want %+v", got, want)
	}

	ptt := drive(t, pushToTalk(), newClock())
	want = []contract.UsageQuantity{
		{Unit: contract.UnitAudioInputMilliseconds, Quantity: 100},
		{Unit: contract.UnitAudioOutputMilliseconds, Quantity: 100},
		{Unit: contract.UnitRequests, Quantity: 1},
	}
	if got := ptt.Measured(); !reflect.DeepEqual(got, want) {
		t.Errorf("push-to-talk measured %+v, want %+v", got, want)
	}
}

// TestXAISessionClockStopsWhenXAIEndsTheSession: a session xAI ends is billed
// until xAI ended it, not until Kaana gets round to settling it.
func TestXAISessionClockStopsWhenXAIEndsTheSession(t *testing.T) {
	clock := newClock()
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) { conn.Close(websocket.StatusNormalClosure) }
	session := xaiOpenWithClock(t, upstream, serverVAD(), clock.Now)
	clock.advance(12 * time.Second)
	if _, err := session.Next(context.Background()); !errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
		t.Fatalf("Next = %v; want the upstream's clean close", err)
	}
	clock.advance(5 * time.Minute) // the session settling late
	want := []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 12_000}}
	if got := session.(provider.RealtimeMeter).Measured(); !reflect.DeepEqual(got, want) {
		t.Errorf("measured %+v, want %+v", got, want)
	}
}

// TestXAIServerVADUpdatesKeepTheBillingMode: once a session is billed by its
// clock it stays so. A switch to push-to-talk, or a text-only session xAI would
// still answer aloud, is refused before anything is written; retuning
// server_vad itself is the control and reaches xAI.
func TestXAIServerVADUpdatesKeepTheBillingMode(t *testing.T) {
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		update := conn.Expect("session.update")
		session, _ := update["session"].(map[string]any)
		if got := asJSON(t, session["turn_detection"]); got != `{"threshold":0.7,"type":"server_vad"}` {
			t.Errorf("retuned turn_detection = %s", got)
		}
	}
	session := xaiOpen(t, upstream, serverVAD())
	for name, update := range map[string]contract.RealtimeSessionConfigUpdate{
		"a switch to push-to-talk": {TurnDetection: &contract.RealtimeTurnDetection{Type: contract.TurnDetectionNone}},
		"text only":                {OutputModalities: []contract.RealtimeOutputModality{contract.RealtimeOutputText}},
	} {
		var unsupported provider.ErrUnsupported
		err := session.Send(context.Background(), &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_refused", Config: update})
		if !errors.As(err, &unsupported) || unsupported.Code != contract.CodeInvalidRequest {
			t.Errorf("%s: Send = %v; want a refusal before anything is written", name, err)
		}
	}
	send(t, session, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_retune", Config: contract.RealtimeSessionConfigUpdate{
		TurnDetection: &contract.RealtimeTurnDetection{Type: contract.TurnDetectionServerVAD, Threshold: pointer(0.7), CreateResponse: pointer(true), InterruptResponse: pointer(true)}}})
	_ = session.Close()
	upstream.WaitClosed(t)
	if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.update", "session.update"}) {
		t.Errorf("xAI received %v; a refused update reached it", types)
	}
}
