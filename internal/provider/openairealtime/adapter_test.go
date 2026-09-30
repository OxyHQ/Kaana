package openairealtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

const requestID contract.RequestID = "req_realtime_adapter"

func pointer[T any](value T) *T { return &value }

func testRoute() provider.Route {
	return provider.Route{
		DeploymentID: "dep_openai_realtime", Provider: Slug,
		ModelReference: "openai/gpt-realtime-2.1@observed-2026-09-30", UpstreamModelID: "gpt-realtime-2.1",
	}
}

func conversationConfig() contract.RealtimeSessionConfig {
	return contract.RealtimeSessionConfig{
		Instructions:      pointer("be brief"),
		OutputModalities:  []contract.RealtimeOutputModality{contract.RealtimeOutputAudio},
		Voice:             pointer("marin"),
		InputAudioFormat:  contract.RealtimePCM16,
		OutputAudioFormat: pointer(contract.RealtimePCM16),
		TurnDetection: contract.RealtimeTurnDetection{
			Type: contract.TurnDetectionServerVAD, Threshold: pointer(0.5), PrefixPaddingMs: pointer(300),
			SilenceDurationMs: pointer(500), CreateResponse: pointer(true), InterruptResponse: pointer(true),
		},
		Tools: []contract.ToolDefinition{{
			Type: "function", Name: "lookup", Description: pointer("find an order"),
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
		}},
		ToolChoice:      &contract.ToolChoice{Mode: pointer(contract.ToolChoiceAuto)},
		MaxOutputTokens: pointer(1024),
	}
}

func openRequest(config contract.RealtimeSessionConfig) provider.RealtimeOpenRequest {
	return provider.RealtimeOpenRequest{RequestID: requestID, Route: testRoute(), Kind: contract.RealtimeConversation, Config: config}
}

func newAdapter(t *testing.T, upstream *fake.Upstream) *Adapter {
	t.Helper()
	adapter, err := New(Config{Declarations: []provider.KeyDeclaration{{KeyID: "key_realtime", Secret: fake.Key}}, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func open(t *testing.T, upstream *fake.Upstream, config contract.RealtimeSessionConfig) provider.RealtimeUpstream {
	t.Helper()
	session, opened, err := newAdapter(t, upstream).Open(context.Background(), openRequest(config), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.KeyID != "key_realtime" {
		t.Fatalf("the open attempt names key %q", opened.KeyID)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func next(t *testing.T, session provider.RealtimeUpstream) provider.RealtimeUpstreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	event, err := session.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return event
}

func send(t *testing.T, session provider.RealtimeUpstream, command contract.RealtimeCommand) {
	t.Helper()
	if err := session.Send(context.Background(), command); err != nil {
		t.Fatalf("Send %s: %v", command.CommandType(), err)
	}
}

func eventTypes(events []provider.RealtimeUpstreamEvent) []contract.RealtimeEventType {
	types := make([]contract.RealtimeEventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Event.EventType())
	}
	return types
}

func asJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(normalized)
	return string(canonical)
}

// TestOpenConfiguresTheSessionOnOpenAIsWire pins the whole configuring
// session.update against OpenAI's GA shape, with every optional field the
// contract carries populated.
func TestOpenConfiguresTheSessionOnOpenAIsWire(t *testing.T) {
	upstream := fake.New(t)
	open(t, upstream, conversationConfig())

	received := upstream.Received()
	if len(received) != 1 {
		t.Fatalf("the fake read %d client events, want exactly the configuring session.update", len(received))
	}
	want := `{"event_id":"kaana-session-open","session":{"audio":{"input":{"format":{"rate":24000,"type":"audio/pcm"},"turn_detection":{"create_response":true,"interrupt_response":true,"prefix_padding_ms":300,"silence_duration_ms":500,"threshold":0.5,"type":"server_vad"}},"output":{"format":{"rate":24000,"type":"audio/pcm"},"voice":"marin"}},"instructions":"be brief","max_output_tokens":1024,"output_modalities":["audio"],"tool_choice":"auto","tools":[{"description":"find an order","name":"lookup","parameters":{"properties":{"id":{"type":"string"}},"type":"object"},"type":"function"}],"type":"realtime"},"type":"session.update"}`
	if got := asJSON(t, received[0]); got != want {
		t.Errorf("session.update =\n%s\nwant\n%s", got, want)
	}
	if models := upstream.Models(); !reflect.DeepEqual(models, []string{"gpt-realtime-2.1"}) {
		t.Errorf("the handshake named models %v; it must name the signed upstream model", models)
	}
}

func TestOpenMapsFormatsAndTurnDetection(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*contract.RealtimeSessionConfig)
		want   string // the session.audio JSON
	}{
		{"g711 with client commits", func(c *contract.RealtimeSessionConfig) {
			c.InputAudioFormat, c.OutputAudioFormat = contract.RealtimeULaw, pointer(contract.RealtimeALaw)
			c.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionNone}
		}, `{"input":{"format":{"type":"audio/pcmu"},"turn_detection":null},"output":{"format":{"type":"audio/pcma"},"voice":"marin"}}`},
		{"semantic vad", func(c *contract.RealtimeSessionConfig) {
			c.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionSemanticVAD, Eagerness: pointer(contract.RealtimeVADEagerness("low")), CreateResponse: pointer(false), InterruptResponse: pointer(true)}
		}, `{"input":{"format":{"rate":24000,"type":"audio/pcm"},"turn_detection":{"create_response":false,"eagerness":"low","interrupt_response":true,"type":"semantic_vad"}},"output":{"format":{"rate":24000,"type":"audio/pcm"},"voice":"marin"}}`},
		{"text only", func(c *contract.RealtimeSessionConfig) {
			c.OutputModalities, c.Voice, c.OutputAudioFormat = []contract.RealtimeOutputModality{contract.RealtimeOutputText}, nil, nil
		}, `{"input":{"format":{"rate":24000,"type":"audio/pcm"},"turn_detection":{"create_response":true,"interrupt_response":true,"prefix_padding_ms":300,"silence_duration_ms":500,"threshold":0.5,"type":"server_vad"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := conversationConfig()
			tc.mutate(&config)
			upstream := fake.New(t)
			open(t, upstream, config)
			session := upstream.Received()[0]["session"].(map[string]any)
			if got := asJSON(t, session["audio"]); got != tc.want {
				t.Errorf("audio =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestWhatOpenAICannotExpressIsRefusedBeforeAnythingIsDialled covers each
// refusal class, and requires that not one handshake reached the upstream.
func TestWhatOpenAICannotExpressIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	cases := map[string]struct {
		mutate func(*provider.RealtimeOpenRequest)
		code   contract.ErrorCode
		param  string
	}{
		"transcription session": {func(r *provider.RealtimeOpenRequest) { r.Kind = contract.RealtimeTranscription }, contract.CodeUnsupportedModality, "kind"},
		"translation session":   {func(r *provider.RealtimeOpenRequest) { r.Kind = contract.RealtimeTranslation }, contract.CodeUnsupportedModality, "kind"},
		"temperature":           {func(r *provider.RealtimeOpenRequest) { r.Config.Temperature = pointer(0.8) }, contract.CodeInvalidRequest, "config.temperature"},
		"input transcription": {func(r *provider.RealtimeOpenRequest) {
			r.Config.InputAudioTranscription = &contract.RealtimeInputTranscription{Language: pointer("en")}
		}, contract.CodeInvalidRequest, "config.inputAudioTranscription"},
		"text and audio together": {func(r *provider.RealtimeOpenRequest) {
			r.Config.OutputModalities = []contract.RealtimeOutputModality{contract.RealtimeOutputText, contract.RealtimeOutputAudio}
		}, contract.CodeInvalidRequest, "config.outputModalities"},
		"too many output tokens": {func(r *provider.RealtimeOpenRequest) { r.Config.MaxOutputTokens = pointer(4097) }, contract.CodeInvalidRequest, "config.maxOutputTokens"},
		"strict tool":            {func(r *provider.RealtimeOpenRequest) { r.Config.Tools[0].Strict = pointer(true) }, contract.CodeInvalidRequest, "config.tools[0].strict"},
		"another slug":           {func(r *provider.RealtimeOpenRequest) { r.Route.Provider = "openai" }, contract.CodeInvalidRequest, "authorizedRoutes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := fake.New(t)
			request := openRequest(conversationConfig())
			tc.mutate(&request)
			_, _, err := newAdapter(t, upstream).Open(context.Background(), request, nil)
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Code != tc.code || unsupported.Param != tc.param {
				t.Fatalf("Open = %v; want %s naming %s", err, tc.code, tc.param)
			}
			if upstream.Dials() != 0 {
				t.Errorf("a refused session dialled OpenAI %d times", upstream.Dials())
			}
		})
	}
	// Positive control: the unmodified request opens on the same fake.
	upstream := fake.New(t)
	open(t, upstream, conversationConfig())
	if upstream.Dials() != 1 {
		t.Fatalf("the control dialled %d times", upstream.Dials())
	}
}

// TestAConversationTurnIsNormalizedInOrder drives one spoken turn through the
// real wire: VAD, the committed item, a response with transcript, audio that
// is larger than one contract frame, and its usage.
func TestAConversationTurnIsNormalizedInOrder(t *testing.T) {
	audio := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 20_000) // 140 000 bytes: three contract frames
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		appended := conn.Expect("input_audio_buffer.append")
		if appended["event_id"] != "cmd_append" || appended["audio"] != "AAAA" {
			t.Errorf("append = %v", appended)
		}
		conn.Send(fake.Event{"type": "input_audio_buffer.speech_started", "audio_start_ms": 120, "item_id": "item_user"})
		conn.Send(fake.Event{"type": "input_audio_buffer.speech_stopped", "audio_end_ms": 940, "item_id": "item_user"})
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_user", "previous_item_id": nil})
		conn.Send(fake.Event{"type": "conversation.item.added", "previous_item_id": nil, "item": fake.Event{"id": "item_user", "type": "message", "role": "user", "status": "completed", "content": []any{fake.Event{"type": "input_audio", "transcript": nil}}}})
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "rate_limits.updated", "rate_limits": []any{}})
		conn.Send(fake.Event{"type": "conversation.item.added", "item": fake.Event{"id": "item_answer", "type": "message", "role": "assistant", "content": []any{}}})
		conn.Send(fake.Event{"type": "response.output_audio_transcript.delta", "response_id": "resp_1", "item_id": "item_answer", "output_index": 0, "content_index": 0, "delta": "Hello"})
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_answer", "output_index": 0, "content_index": 0, "delta": base64.StdEncoding.EncodeToString(audio)})
		conn.Send(fake.Event{"type": "response.output_audio.done", "response_id": "resp_1", "item_id": "item_answer", "output_index": 0, "content_index": 0})
		conn.Send(fake.Event{"type": "response.output_audio_transcript.done", "response_id": "resp_1", "item_id": "item_answer", "output_index": 0, "content_index": 0, "transcript": "Hello"})
		conn.Send(fake.Event{"type": "conversation.item.done", "item": fake.Event{"id": "item_answer", "type": "message", "role": "assistant", "content": []any{fake.Event{"type": "output_audio", "transcript": "Hello"}}}})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "output": []any{fake.Event{"type": "message"}}, "usage": fake.Usage(119, 13, 64, 0, 30, 91)}})
	}
	session := open(t, upstream, conversationConfig())
	send(t, session, &contract.RealtimeInputAudioAppendCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_append", Data: "AAAA"})

	var events []provider.RealtimeUpstreamEvent
	for len(events) == 0 || events[len(events)-1].Event.EventType() != contract.RealtimeEventResponseDoneType {
		events = append(events, next(t, session))
	}
	want := []contract.RealtimeEventType{
		contract.RealtimeEventSpeechStartedType, contract.RealtimeEventSpeechStoppedType, contract.RealtimeEventInputAudioCommittedType,
		contract.RealtimeEventItemAddedType, contract.RealtimeEventResponseCreatedType, contract.RealtimeEventTranscriptDeltaType,
		contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventOutputAudioDeltaType,
		contract.RealtimeEventOutputAudioDoneType, contract.RealtimeEventTranscriptDoneType, contract.RealtimeEventItemDoneType,
		contract.RealtimeEventResponseDoneType,
	}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}

	started := events[0].Event.(*contract.RealtimeSpeechStartedEvent)
	if started.ItemID != "item_user" || started.AudioStartMs != 120 {
		t.Errorf("speech_started = %+v", started)
	}
	added := events[3].Event.(*contract.RealtimeItemAddedEvent)
	if added.Item.Content[0].Type != contract.RealtimeInputAudioPart || *added.Item.Content[0].Format != contract.RealtimePCM16 || added.Item.Content[0].Data != nil {
		t.Errorf("the committed user item = %+v", added.Item)
	}
	var reassembled []byte
	for _, frame := range events[6:9] {
		delta := frame.Event.(*contract.RealtimeOutputAudioDeltaEvent)
		if err := contract.ValidateRealtimeAudioFrame(delta.Data); err != nil {
			t.Fatalf("a split frame is not a contract frame: %v", err)
		}
		decoded, _ := base64.StdEncoding.DecodeString(delta.Data)
		reassembled = append(reassembled, decoded...)
		if delta.ResponseID != "resp_1" || delta.ItemID != "item_answer" || delta.Format != contract.RealtimePCM16 {
			t.Errorf("frame = %+v", delta)
		}
	}
	if !bytes.Equal(reassembled, audio) {
		t.Error("the split frames do not concatenate to the audio OpenAI sent")
	}
	transcript := events[5].Event.(*contract.RealtimeTranscriptDeltaEvent)
	if transcript.Source != contract.RealtimeTranscriptOutput || transcript.Text != "Hello" || *transcript.ResponseID != "resp_1" {
		t.Errorf("transcript delta = %+v", transcript)
	}
	done := events[12]
	response := done.Event.(*contract.RealtimeResponseDoneEvent)
	wantUnits := []contract.UsageQuantity{
		{Unit: contract.UnitInputTokens, Quantity: 55}, {Unit: contract.UnitCachedInputTokens, Quantity: 64},
		{Unit: contract.UnitAudioInputTokens, Quantity: 13}, {Unit: contract.UnitCachedAudioInputTokens, Quantity: 0},
		{Unit: contract.UnitOutputTokens, Quantity: 30}, {Unit: contract.UnitAudioOutputTokens, Quantity: 91},
	}
	if response.Status != contract.RealtimeResponseCompleted || *response.FinishReason != contract.FinishStop ||
		!reflect.DeepEqual(response.Units, wantUnits) || !reflect.DeepEqual(done.Units, wantUnits) || response.UsageSource != contract.UsageProviderReported {
		t.Errorf("response.done = %+v units %v", response, done.Units)
	}
}

// TestTheAudioChunkBoundIsTheRealtimeFrameBound: splitting at the shared
// chunk bound must yield frames the contract accepts, and no smaller.
func TestTheAudioChunkBoundIsTheRealtimeFrameBound(t *testing.T) {
	if provider.MaxAudioChunkBytes != contract.MaxRealtimeAudioFrameBase64Length/4*3 {
		t.Fatalf("MaxAudioChunkBytes %d is not the decoded size of a %d-character frame", provider.MaxAudioChunkBytes, contract.MaxRealtimeAudioFrameBase64Length)
	}
	if err := contract.ValidateRealtimeAudioFrame(base64.StdEncoding.EncodeToString(make([]byte, maxAudioFrameBytes))); err != nil {
		t.Fatalf("a full frame is refused: %v", err)
	}
}

// TestUsagePartitionsCachedAudio uses cached audio as well as cached text, the
// case where subtracting only one of them double-charges the other.
func TestUsagePartitionsCachedAudio(t *testing.T) {
	var value usage
	raw, _ := json.Marshal(fake.Usage(100, 400, 40, 150, 20, 300))
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	units, ok := value.units()
	if !ok {
		t.Fatal("a consistent report was refused")
	}
	total := 0
	for _, unit := range units {
		total += unit.Quantity
	}
	if total != 100+400+20+300 {
		t.Errorf("units %v sum to %d; they must partition the %d tokens OpenAI reported", units, total, 820)
	}
	want := map[contract.UsageUnit]int{
		contract.UnitInputTokens: 60, contract.UnitCachedInputTokens: 40, contract.UnitAudioInputTokens: 250,
		contract.UnitCachedAudioInputTokens: 150, contract.UnitOutputTokens: 20, contract.UnitAudioOutputTokens: 300,
	}
	for _, unit := range units {
		if want[unit.Unit] != unit.Quantity {
			t.Errorf("%s = %d, want %d", unit.Unit, unit.Quantity, want[unit.Unit])
		}
	}
	value.InputTokenDetails.CachedTokensDetails.AudioTokens = 500
	if _, ok := value.units(); ok {
		t.Error("cached audio larger than the audio input was partitioned instead of refused")
	}
}

// TestVADInterruptionAndTruncation is the barge-in: speech starts while the
// model is speaking, the response is cancelled by turn detection, and the
// client truncates what was not heard.
func TestVADInterruptionAndTruncation(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress"}})
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0, "delta": "AAAAAAAA"})
		conn.Send(fake.Event{"type": "input_audio_buffer.speech_started", "audio_start_ms": 2000, "item_id": "item_user_2"})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "cancelled", "status_details": fake.Event{"type": "cancelled", "reason": "turn_detected"}, "usage": fake.Usage(10, 20, 0, 0, 3, 40)}})
		truncate := conn.Expect("conversation.item.truncate")
		if truncate["item_id"] != "item_answer" || truncate["content_index"] != float64(0) || truncate["audio_end_ms"] != float64(640) || truncate["event_id"] != "cmd_truncate" {
			t.Errorf("truncate = %v", truncate)
		}
		conn.Send(fake.Event{"type": "conversation.item.truncated", "item_id": "item_answer", "content_index": 0, "audio_end_ms": 640})
	}
	session := open(t, upstream, conversationConfig())
	types := []contract.RealtimeEventType{}
	var cancelled *contract.RealtimeResponseDoneEvent
	for len(types) < 4 {
		event := next(t, session)
		types = append(types, event.Event.EventType())
		if done, ok := event.Event.(*contract.RealtimeResponseDoneEvent); ok {
			cancelled = done
		}
	}
	if !reflect.DeepEqual(types, []contract.RealtimeEventType{contract.RealtimeEventResponseCreatedType, contract.RealtimeEventOutputAudioDeltaType, contract.RealtimeEventSpeechStartedType, contract.RealtimeEventResponseDoneType}) {
		t.Fatalf("events = %v", types)
	}
	if cancelled.Status != contract.RealtimeResponseCancelled || *cancelled.FinishReason != contract.FinishCancelled || len(cancelled.Units) == 0 {
		t.Errorf("an interrupted response still consumed what it consumed: %+v", cancelled)
	}
	send(t, session, &contract.RealtimeItemTruncateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_truncate", ItemID: "item_answer", ContentIndex: 0, AudioEndMs: 640})
	truncated := next(t, session).Event.(*contract.RealtimeItemTruncatedEvent)
	if truncated.ItemID != "item_answer" || truncated.AudioEndMs != 640 {
		t.Errorf("truncated = %+v", truncated)
	}
}

// TestToolCallsStreamUnderTheOneShotRules covers a streamed call (name first,
// arguments as deltas, completion without repeating them) and an unstreamed
// one (arguments only in the done event), then the client answering.
func TestToolCallsStreamUnderTheOneShotRules(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "response.output_item.added", "response_id": "resp_1", "output_index": 0, "item": fake.Event{"id": "item_call", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": ""}})
		conn.Send(fake.Event{"type": "response.function_call_arguments.delta", "response_id": "resp_1", "item_id": "item_call", "output_index": 0, "call_id": "call_1", "delta": `{"id":`})
		conn.Send(fake.Event{"type": "response.function_call_arguments.delta", "response_id": "resp_1", "item_id": "item_call", "output_index": 0, "call_id": "call_1", "delta": `"A1"}`})
		conn.Send(fake.Event{"type": "response.function_call_arguments.done", "response_id": "resp_1", "item_id": "item_call", "output_index": 0, "call_id": "call_1", "name": "lookup", "arguments": `{"id":"A1"}`})
		conn.Send(fake.Event{"type": "response.function_call_arguments.done", "response_id": "resp_1", "item_id": "item_call_2", "output_index": 1, "call_id": "call_2", "name": "lookup", "arguments": `{}`})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "output": []any{fake.Event{"type": "function_call"}}, "usage": fake.Usage(10, 0, 0, 0, 12, 0)}})
		created := conn.Expect("conversation.item.create")
		wantItem := `{"call_id":"call_1","output":"{\"status\":\"shipped\"}","type":"function_call_output"}`
		if got := asJSON(t, created["item"]); got != wantItem {
			t.Errorf("function output item = %s, want %s", got, wantItem)
		}
		conn.Expect("response.create")
	}
	session := open(t, upstream, conversationConfig())
	var calls []*contract.RealtimeToolCallEvent
	var done *contract.RealtimeResponseDoneEvent
	for done == nil {
		switch event := next(t, session).Event.(type) {
		case *contract.RealtimeToolCallEvent:
			calls = append(calls, event)
		case *contract.RealtimeResponseDoneEvent:
			done = event
		}
	}
	if len(calls) != 4 {
		t.Fatalf("%d tool call events, want 4", len(calls))
	}
	if calls[0].Name == nil || *calls[0].Name != "lookup" || *calls[0].ArgumentsDelta != `{"id":` || calls[0].Complete {
		t.Errorf("first increment = %+v", calls[0])
	}
	if calls[1].Name != nil || *calls[1].ArgumentsDelta != `"A1"}` {
		t.Errorf("second increment = %+v", calls[1])
	}
	if !calls[2].Complete || calls[2].ArgumentsDelta != nil || calls[2].Name != nil {
		t.Errorf("a streamed call's completion repeated what was streamed: %+v", calls[2])
	}
	if !calls[3].Complete || calls[3].ArgumentsDelta == nil || *calls[3].ArgumentsDelta != `{}` || *calls[3].Name != "lookup" || calls[3].ToolCallID != "call_2" {
		t.Errorf("an unstreamed call must carry its name and arguments on completion: %+v", calls[3])
	}
	if *done.FinishReason != contract.FinishToolCalls {
		t.Errorf("finish = %s", *done.FinishReason)
	}
	send(t, session, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_output", Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeFunctionCallOutputItem, CallID: pointer("call_1"), Output: pointer(`{"status":"shipped"}`),
	}})
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})
	_ = session.Close()
	upstream.WaitClosed(t)
}

func TestResponseCancelNamesTheResponse(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		created := conn.Expect("response.create")
		want := `{"event_id":"cmd_create","response":{"instructions":"shorter","max_output_tokens":64,"output_modalities":["text"],"tool_choice":{"name":"lookup","type":"function"}},"type":"response.create"}`
		if got := asJSON(t, created); got != want {
			t.Errorf("response.create =\n%s\nwant\n%s", got, want)
		}
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_9", "status": "in_progress"}})
		conn.Send(fake.Event{"type": "response.output_text.delta", "response_id": "resp_9", "item_id": "item_9", "content_index": 0, "delta": "Sure"})
		cancel := conn.Expect("response.cancel")
		if cancel["response_id"] != "resp_9" || cancel["event_id"] != "cmd_cancel" {
			t.Errorf("cancel = %v", cancel)
		}
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_9", "status": "cancelled", "status_details": fake.Event{"type": "cancelled", "reason": "client_cancelled"}}})
	}
	session := open(t, upstream, conversationConfig())
	send(t, session, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_create", Response: &contract.RealtimeResponseParameters{
		Instructions: pointer("shorter"), OutputModalities: []contract.RealtimeOutputModality{contract.RealtimeOutputText}, MaxOutputTokens: pointer(64),
		ToolChoice: &contract.ToolChoice{Function: &contract.ToolChoiceFunction{Type: "function", Name: "lookup"}},
	}})
	if next(t, session).Event.EventType() != contract.RealtimeEventResponseCreatedType {
		t.Fatal("expected response.created")
	}
	text := next(t, session).Event.(*contract.RealtimeTextDeltaEvent)
	if text.Text != "Sure" || text.ResponseID != "resp_9" {
		t.Errorf("text delta = %+v", text)
	}
	responseID := contract.RealtimeResponseID("resp_9")
	send(t, session, &contract.RealtimeResponseCancelCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_cancel", ResponseID: &responseID})
	done := next(t, session).Event.(*contract.RealtimeResponseDoneEvent)
	if done.Status != contract.RealtimeResponseCancelled || len(done.Units) != 0 || done.Units == nil {
		t.Errorf("a response with no usage reports an empty, non-null unit list: %+v", done)
	}
}

// TestSessionUpdateIsConfirmedWithTheEffectiveConfiguration also covers a
// refused update: its error names the command, and the next confirmation is
// matched to the update that was actually applied.
func TestSessionUpdateIsConfirmedWithTheEffectiveConfiguration(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		first := conn.Expect("session.update")
		conn.Send(fake.Event{"type": "error", "error": fake.Event{"type": "invalid_request_error", "code": "invalid_value", "message": "bad instructions", "param": "session.instructions", "event_id": first["event_id"]}})
		second := conn.Expect("session.update")
		if second["session"].(map[string]any)["instructions"] != "be warmer" {
			t.Errorf("second update = %v", second)
		}
		conn.Send(fake.Event{"type": "session.updated", "session": second["session"]})
	}
	session := open(t, upstream, conversationConfig())
	send(t, session, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_bad", Config: contract.RealtimeSessionConfigUpdate{Instructions: pointer("x")}})
	refusal := next(t, session)
	failure := refusal.Event.(*contract.RealtimeErrorEvent)
	if refusal.CommandID == nil || *refusal.CommandID != "cmd_bad" || failure.Fatal || failure.Error.Code != contract.CodeInvalidRequest {
		t.Fatalf("the refused update = %+v (command %v)", failure, refusal.CommandID)
	}
	send(t, session, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_good", Config: contract.RealtimeSessionConfigUpdate{Instructions: pointer("be warmer")}})
	confirmed := next(t, session)
	updated := confirmed.Event.(*contract.RealtimeSessionUpdatedEvent)
	if *confirmed.CommandID != "cmd_good" || *updated.Config.Instructions != "be warmer" || updated.Config.Voice == nil || *updated.Config.Voice != "marin" {
		t.Errorf("session.updated = %+v for %v", updated.Config, confirmed.CommandID)
	}
}

func TestCommandsOpenAICannotExpressAreRefusedWithoutSending(t *testing.T) {
	upstream := fake.New(t)
	session := open(t, upstream, conversationConfig())
	refusals := map[string]contract.RealtimeCommand{
		"assistant audio": &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "c1", Item: contract.RealtimeConversationItem{
			Type: contract.RealtimeMessageItem, Role: pointer(contract.RealtimeItemRole("assistant")),
			Content: []contract.RealtimeContentPart{{Type: contract.RealtimeOutputAudioPart, Format: pointer(contract.RealtimePCM16)}},
		}},
		"audio in another format": &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "c2", Item: contract.RealtimeConversationItem{
			Type: contract.RealtimeMessageItem, Role: pointer(contract.RealtimeItemRole("user")),
			Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputAudioPart, Format: pointer(contract.RealtimeULaw), Data: pointer("AAAA")}},
		}},
		"temperature":   &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "c3", Config: contract.RealtimeSessionConfigUpdate{Temperature: pointer(1.0)}},
		"session.close": &contract.RealtimeSessionCloseCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "c4"},
	}
	for name, command := range refusals {
		var unsupported provider.ErrUnsupported
		if err := session.Send(context.Background(), command); !errors.As(err, &unsupported) {
			t.Errorf("%s: Send = %v, want a refusal", name, err)
		}
	}
	_ = session.Close()
	upstream.WaitClosed(t)
	if received := upstream.ReceivedTypes(); !reflect.DeepEqual(received, []string{"session.update"}) {
		t.Errorf("refused commands reached OpenAI: %v", received)
	}
}

// TestAProviderFailureMidSessionEndsItAsAnUpstreamError: OpenAI reports its
// own failure and drops the connection. The error is delivered, then the
// session reports the failure it ended with — never a clean close.
func TestAProviderFailureMidSessionEndsItAsAnUpstreamError(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "error", "error": fake.Event{"type": "server_error", "code": nil, "message": "The server had an error while processing your request, key " + fake.Key, "event_id": nil}})
		conn.Close(websocket.StatusInternalError)
	}
	session := open(t, upstream, conversationConfig())
	event := next(t, session)
	failure := event.Event.(*contract.RealtimeErrorEvent)
	if failure.Fatal || failure.Error.Code != contract.CodeProviderError || event.CommandID != nil {
		t.Fatalf("error = %+v", failure)
	}
	message := *failure.Error.ProviderError.Message
	if strings.Contains(message, fake.Key) || !strings.Contains(message, "[redacted]") {
		t.Errorf("the platform key reached the customer, or the diagnostic was lost: %q", message)
	}
	_, err := session.Next(context.Background())
	var upstreamFailure provider.ErrUpstream
	if !errors.As(err, &upstreamFailure) || upstreamFailure.Code != contract.CodeProviderError || errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
		t.Errorf("the session ended with %v; it must end with OpenAI's reported failure", err)
	}
}

func TestACleanCloseIsTheUpstreamEndingTheSession(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) { conn.Close(websocket.StatusNormalClosure) }
	session := open(t, upstream, conversationConfig())
	if _, err := session.Next(context.Background()); !errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
		t.Errorf("Next = %v, want a clean upstream close", err)
	}
	// Control: an abnormal drop is not reported as a clean close.
	dropped := fake.New(t)
	dropped.Script = func(conn *fake.Conn) { conn.Drop() }
	session = open(t, dropped, conversationConfig())
	if _, err := session.Next(context.Background()); err == nil || errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
		t.Errorf("an abnormal drop reported %v", err)
	}
}

func TestInconsistentUsageEndsTheSessionRatherThanSettlingAGuess(t *testing.T) {
	upstream := fake.New(t)
	bad := fake.Usage(10, 0, 20, 0, 1, 0) // more cached than input
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "usage": bad}})
	}
	session := open(t, upstream, conversationConfig())
	if _, err := session.Next(context.Background()); err == nil {
		t.Error("inconsistent usage was settled")
	}
}

// TestRefusedHandshakesAreClassifiedFromOpenAIsOwnVocabulary covers the
// credential verdicts through the real pool: a refused key and an exhausted
// account retire it, a throttle does not, and the key never reaches the text.
func TestRefusedHandshakesAreClassifiedFromOpenAIsOwnVocabulary(t *testing.T) {
	cases := map[string]struct {
		status  int
		body    string
		code    contract.ErrorCode
		retired bool
	}{
		"invalid key":        {http.StatusUnauthorized, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided: ` + fake.Key + `"}}`, contract.CodeProviderCredentialInvalid, true},
		"insufficient quota": {http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota"}}`, contract.CodeProviderBillingRefused, true},
		"rate limit":         {http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`, contract.CodeRateLimited, false},
		"overloaded":         {http.StatusServiceUnavailable, `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"busy"}}`, contract.CodeProviderOverloaded, false},
		"unknown model":      {http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"no such model"}}`, contract.CodeModelNotFound, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := fake.New(t)
			upstream.Refuse = func(w http.ResponseWriter, _ *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
				return true
			}
			adapter := newAdapter(t, upstream)
			_, opened, err := adapter.Open(context.Background(), openRequest(conversationConfig()), nil)
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("Open = %v, want %s", err, tc.code)
			}
			if opened.KeyID != "key_realtime" {
				t.Errorf("a refused attempt must still name the key it spent, got %q", opened.KeyID)
			}
			if tc.code == contract.CodeRateLimited && failure.RetryAfterMs != 2000 {
				t.Errorf("the throttle lost OpenAI's Retry-After: %d", failure.RetryAfterMs)
			}
			if failure.Passthrough != nil && failure.Passthrough.Message != nil && strings.Contains(*failure.Passthrough.Message, fake.Key) {
				t.Error("the platform key reached the error text")
			}
			if name == "invalid key" && !strings.Contains(*failure.Passthrough.Message, "[redacted]") {
				t.Error("control: the upstream echoed the key, so the diagnostic must survive redacted")
			}
			retired := adapter.PlatformCredentials().Projection(time.Now()).Usable == 0
			if retired != tc.retired {
				t.Errorf("retired = %v, want %v", retired, tc.retired)
			}
			if upstream.Dials() != 1 {
				t.Errorf("a one-key view dialled %d times", upstream.Dials())
			}
		})
	}
}

// TestAConfigurationRefusalIsAnAnswerAboutTheSession: the handshake succeeded,
// but OpenAI refused what the session asked for, or the account behind it.
func TestAConfigurationRefusalIsAnAnswerAboutTheSession(t *testing.T) {
	cases := map[string]struct {
		refusal fake.Event
		code    contract.ErrorCode
		retired bool
	}{
		"invalid configuration": {fake.Event{"type": "error", "error": fake.Event{"type": "invalid_request_error", "code": "invalid_value", "message": "voice", "event_id": "kaana-session-open"}}, contract.CodeInvalidRequest, false},
		"exhausted account":     {fake.Event{"type": "error", "error": fake.Event{"type": "insufficient_quota", "code": "insufficient_quota", "message": "quota"}}, contract.CodeProviderBillingRefused, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := fake.New(t)
			upstream.Configure = func(fake.Event) fake.Event { return tc.refusal }
			adapter := newAdapter(t, upstream)
			_, _, err := adapter.Open(context.Background(), openRequest(conversationConfig()), nil)
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("Open = %v, want %s", err, tc.code)
			}
			if retired := adapter.PlatformCredentials().Projection(time.Now()).Usable == 0; retired != tc.retired {
				t.Errorf("retired = %v, want %v", retired, tc.retired)
			}
			upstream.WaitClosed(t)
		})
	}
}

// TestCancellationReachesTheUpstream: the caller withdrawing while OpenAI has
// not confirmed the session ends the attempt, with an uncancelled control.
func TestCancellationReachesTheUpstream(t *testing.T) {
	release := make(chan struct{})
	upstream := fake.New(t)
	upstream.Configure = func(fake.Event) fake.Event {
		<-release
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, _, err := newAdapter(t, upstream).Open(ctx, openRequest(conversationConfig()), nil)
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled open returned %v", err)
	}
	upstream.WaitClosed(t)

	control := fake.New(t)
	open(t, control, conversationConfig())
}

func TestTheRegistryAcceptsTheAdapterOnlyAsASessionAdapter(t *testing.T) {
	adapter, err := New(Config{Declarations: provider.DeclareKeys([]string{fake.Key})})
	if err != nil {
		t.Fatal(err)
	}
	var registrant provider.Registrant = adapter
	if _, oneShot := registrant.(provider.Adapter); oneShot {
		t.Fatal("the realtime adapter also implements provider.Adapter, so a text request could be routed to it")
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveExecution("dep", Slug, false); err == nil {
		t.Error("a request resolved to the realtime adapter")
	}
	if !reflect.DeepEqual(adapter.RealtimeSessionKinds(), []contract.RealtimeSessionKind{contract.RealtimeConversation}) {
		t.Errorf("declared kinds = %v", adapter.RealtimeSessionKinds())
	}
}
