package publicapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// dialRealtime connects the way @kaana.ai/sdk does: the credential in the
// kaana.bearer. subprotocol, the model in ?model=, and the first frame.
func (h *harness) dialRealtime(credential string, first any) *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/realtime?model=" + string(realtimeModel.ModelID())
	ws, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{"kaana.v1", "kaana.bearer." + base64.RawURLEncoding.EncodeToString([]byte(credential))},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	if ws.Subprotocol() != "kaana.v1" {
		h.t.Errorf("the server selected %q, not kaana.v1", ws.Subprotocol())
	}
	ws.SetReadLimit(32 << 20)
	h.t.Cleanup(func() { _ = ws.CloseNow() })
	encoded, err := json.Marshal(first)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, encoded); err != nil {
		h.t.Fatalf("first frame: %v", err)
	}
	return ws
}

type event struct {
	raw  []byte
	head struct {
		Type      string             `json:"type"`
		Sequence  int                `json:"sequence"`
		RequestID contract.RequestID `json:"requestId"`
	}
}

func readEvent(t *testing.T, ws *websocket.Conn) event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("reading an event: %v", err)
	}
	var read event
	read.raw = data
	if err := json.Unmarshal(data, &read.head); err != nil {
		t.Fatalf("an event is not JSON: %s", data)
	}
	assertNoProviderLeak(t, "a public realtime event", data)
	return read
}

func readUntil(t *testing.T, ws *websocket.Conn, eventType string) []event {
	t.Helper()
	var events []event
	for {
		read := readEvent(t, ws)
		events = append(events, read)
		if read.head.Type == eventType {
			return events
		}
	}
}

func sendCommand(t *testing.T, ws *websocket.Conn, command map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(command)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

func conversationOpen(extra map[string]any) map[string]any {
	config := map[string]any{
		"instructions": "be brief", "outputModalities": []string{"text"}, "inputAudioFormat": "pcm16_24khz",
		"turnDetection": map[string]any{"type": "none"},
		"tools":         []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	}
	for key, value := range extra {
		config[key] = value
	}
	return map[string]any{"type": "session.open", "kind": "conversation", "config": config, "limits": map[string]any{"maxDurationMs": 120000}}
}

// A realtime session with tools, end to end: granted by Oxy for the model,
// opened on the deployment, its events projected onto the public shapes, a
// tool call answered by the client, closed by the client with its totals, and
// settled once with its provider cost in the same outbox write.
func TestARealtimeSessionWithToolsIsGrantedServedAndSettled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "response.output_item.added", "response_id": "resp_1", "output_index": 0, "item": fake.Event{"id": "item_call", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": ""}})
		conn.Send(fake.Event{"type": "response.function_call_arguments.done", "response_id": "resp_1", "item_id": "item_call", "output_index": 0, "call_id": "call_1", "name": "lookup", "arguments": `{"id":"A1"}`})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "output": []any{fake.Event{"type": "function_call"}}, "usage": fake.Usage(10, 0, 0, 0, 12, 0)}})
		created := conn.Expect("conversation.item.create")
		if item, _ := created["item"].(map[string]any); item["type"] != "function_call_output" || item["call_id"] != "call_1" {
			t.Errorf("the tool answer reached the provider as %v", created)
		}
		conn.Expect("response.create")
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_2", "status": "completed", "output": []any{}, "usage": fake.Usage(4, 0, 0, 0, 3, 0)}})
	}
	ws := h.dialRealtime(validKey, conversationOpen(map[string]any{"inputAudioTranscription": map[string]any{"language": "es"}}))

	created := readEvent(t, ws)
	var session contract.RealtimePublicSessionCreatedEvent
	if err := json.Unmarshal(created.raw, &session); err != nil || created.head.Type != "session.created" || created.head.Sequence != 0 ||
		session.Model != realtimeModel || session.Limits.MaxDurationMs != 120000 {
		t.Fatalf("session.created = %s", created.raw)
	}
	requestID := session.RequestID
	events := readUntil(t, ws, "response.done")
	sawCall := false
	for _, read := range events {
		if read.head.Type == "tool_call" && strings.Contains(string(read.raw), `"complete":true`) {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("no completed tool call among %d events", len(events))
	}
	sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": requestID, "commandId": "cmd_answer", "type": "conversation.item.create",
		"item": map[string]any{"type": "function_call_output", "callId": "call_1", "output": `{"status":"shipped"}`}})
	sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": requestID, "commandId": "cmd_respond", "type": "response.create"})
	readUntil(t, ws, "response.done")
	sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": requestID, "commandId": "cmd_close", "type": "session.close"})
	closedEvents := readUntil(t, ws, "session.closed")
	var closed contract.RealtimePublicSessionClosedEvent
	_ = json.Unmarshal(closedEvents[len(closedEvents)-1].raw, &closed)
	if closed.Reason != contract.RealtimeClosedByClient || len(closed.Units) == 0 {
		t.Fatalf("session.closed = %s", closedEvents[len(closedEvents)-1].raw)
	}
	// No usage report frame follows on the public surface: the socket closes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, data, err := ws.Read(ctx); err == nil {
		t.Errorf("a frame followed session.closed: %s", data)
	}

	var asked contract.InferenceAuthorizationRequest
	_ = json.Unmarshal([]byte(h.oxy.authorizationBodies()[0]), &asked)
	if asked.Operation != contract.GrantRealtimeSession || asked.Facts.RealtimeSessionKind == nil || asked.Facts.DeclaredTools != 1 || asked.RequestID != requestID {
		t.Errorf("authorization = %+v", asked)
	}
	if strings.Contains(h.oxy.authorizationBodies()[0], "be brief") || strings.Contains(h.oxy.authorizationBodies()[0], "lookup") {
		t.Error("the authorization carried the session's instructions or tools")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(h.outbox.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.settle()
	settlement, ok := h.oxy.settled()["grt_1"]
	if !ok || settlement.Report.Outcome != contract.OutcomeCompleted || settlement.ServedStep == nil || settlement.Report.RequestID != requestID {
		t.Fatalf("settlement = %+v", settlement)
	}
	rows := h.outbox.snapshot()
	if len(rows) != 1 || len(rows[0].events) == 0 {
		t.Fatalf("the session's provider cost was not written with its settlement: %+v", rows)
	}
}

// A session Oxy does not grant is one fatal public error, outside any
// sequence, and nothing is dialled upstream.
func TestARealtimeSessionWithoutAGrantIsOneErrorAndNoUpstream(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ws := h.dialRealtime(revokedKey, conversationOpen(nil))
	refused := readEvent(t, ws)
	var failure contract.RealtimePublicErrorEvent
	_ = json.Unmarshal(refused.raw, &failure)
	if refused.head.Type != "error" || refused.head.Sequence != 0 || !failure.Fatal || failure.Error.Code != "authentication_failed" {
		t.Fatalf("refusal = %s", refused.raw)
	}
	if h.upstream.Dials() != 0 {
		t.Error("a provider session was dialled without a grant")
	}
}

// A session the model cannot hold is refused before Oxy is asked: an
// undeclared voice, a language it does not declare.
func TestARealtimeOpenIsCheckedAgainstTheModelBeforeAnyGrant(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for param, extra := range map[string]map[string]any{
		"config.voice": {"voice": "nobody", "outputModalities": []string{"audio"}, "outputAudioFormat": "pcm16_24khz"},
		"config.inputAudioTranscription.language": {"inputAudioTranscription": map[string]any{"language": "fr"}},
	} {
		ws := h.dialRealtime(validKey, conversationOpen(extra))
		refused := readEvent(t, ws)
		var failure contract.RealtimePublicErrorEvent
		_ = json.Unmarshal(refused.raw, &failure)
		if failure.Error.Code != "invalid_request" || failure.Error.Param == nil || *failure.Error.Param != param {
			t.Errorf("%s: %s", param, refused.raw)
		}
	}
	if len(h.oxy.authorizationBodies()) != 0 {
		t.Errorf("%d authorizations for opens the model refuses", len(h.oxy.authorizationBodies()))
	}
}

// A dropped connection resumes only for the credential that opened it.
func TestARealtimeResumeBelongsToTheCredentialThatOpened(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.upstream.Script = func(conn *fake.Conn) { conn.WaitForClose() }
	ws := h.dialRealtime(validKey, conversationOpen(nil))
	created := readEvent(t, ws)
	_ = ws.CloseNow()

	resume := map[string]any{"schemaVersion": 1, "requestId": created.head.RequestID, "commandId": "cmd_resume", "type": "session.resume", "afterSequence": 0}
	stranger := h.dialRealtime(otherKey, resume)
	refused := readEvent(t, stranger)
	if refused.head.Type != "error" || !strings.Contains(string(refused.raw), `"fatal":true`) {
		t.Fatalf("another credential resumed: %s", refused.raw)
	}
	owner := h.dialRealtime(validKey, resume)
	if resumed := readUntil(t, owner, "session.resumed"); len(resumed) == 0 {
		t.Fatal("the opener could not resume")
	}
	sendCommand(t, owner, map[string]any{"schemaVersion": 1, "requestId": created.head.RequestID, "commandId": "cmd_close", "type": "session.close"})
	readUntil(t, owner, "session.closed")
}

// GET /v1/realtime without a WebSocket upgrade is not a session.
func TestRealtimeNeedsAnUpgrade(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	request, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/realtime?model=openai/gpt-realtime-2.1", nil)
	if response := h.do(request); response.StatusCode == http.StatusSwitchingProtocols || response.StatusCode == http.StatusOK {
		t.Errorf("a plain GET was answered %d", response.StatusCode)
	}
}
