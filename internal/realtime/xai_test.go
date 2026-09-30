package realtime_test

import (
	"encoding/base64"
	"reflect"
	"testing"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider/openairealtime"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// xaiSessionRequest is a push-to-talk session on the xAI Voice Agent
// deployments: the one turn detection whose billing the contract can carry.
func xaiSessionRequest(requestID contract.RequestID) contract.RealtimeSessionRequest {
	request := sessionRequest(requestID, primary)
	request.AuthorizedRoutes[0].Provider = openairealtime.XAISlug
	request.Config.Voice = pointer("eve")
	return request
}

// TestAnXAISessionSettlesWhatKaanaMeasured runs an xAI Voice Agent session
// through the whole engine: the units on session.closed, on the usage report
// and on the priced operator record are the audio Kaana sent and received and
// the billed text item, labelled oxy_measured, and none of the tokens xAI
// reported. TestASessionOpensStreamsAndSettlesExactlyOnce is the
// provider_reported control on the same engine.
func TestAnXAISessionSettlesWhatKaanaMeasured(t *testing.T) {
	h := newHarness(t, options{xai: true})
	spoken := make([]byte, 4800) // 100 ms of 24 kHz PCM16
	h.upstream.Script = func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		conn.Expect("input_audio_buffer.commit")
		conn.Expect("response.create")
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_1", "content_index": 0, "delta": base64.StdEncoding.EncodeToString(spoken)})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "completed", "usage": fake.Event{"input_tokens": 500, "output_tokens": 300, "total_tokens": 800}}})
		conn.Expect("conversation.item.create")
	}
	const requestID = contract.RequestID("req_xai_voice")
	ws := h.open(xaiSessionRequest(requestID))
	if created := read(t, ws); created.kind() != "session.created" || created["servingProvider"] != "xai-realtime" {
		t.Fatalf("session.created = %v", created)
	}
	command(t, ws, appendAudio(requestID, "cmd_append", base64.StdEncoding.EncodeToString(make([]byte, 9600)))) // 200 ms
	command(t, ws, &contract.RealtimeInputAudioCommitCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_commit"})
	command(t, ws, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_respond"})
	turn := readUntil(t, ws, "response.done")
	if done := turn[len(turn)-1]; len(unitsOf(done["units"])) != 0 || done["usageSource"] != "oxy_measured" {
		t.Fatalf("response.done = %v; xAI's tokens are not what it bills", done)
	}
	user, text := contract.RealtimeItemRole("user"), "and the order?"
	command(t, ws, &contract.RealtimeItemCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_text", Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeMessageItem, Role: &user, Content: []contract.RealtimeContentPart{{Type: contract.RealtimeInputTextPart, Text: &text}}}})
	readUntil(t, ws, "command.accepted")
	waitFor(t, "the item to reach xAI", func() bool { return len(h.upstream.Received()) == 5 })
	command(t, ws, closeCommand(requestID, "cmd_close"))
	closed := readUntil(t, ws, "session.closed")
	final := closed[len(closed)-1]
	want := map[string]int{"audio_input_milliseconds": 200, "audio_output_milliseconds": 100, "requests": 1}
	if !reflect.DeepEqual(unitsOf(final["units"]), want) || final["usageSource"] != "oxy_measured" {
		t.Fatalf("session.closed = %v", final)
	}
	report := read(t, ws)
	if report["outcome"] != "completed" || report["servingProvider"] != "xai-realtime" || report["usageSource"] != "oxy_measured" ||
		!reflect.DeepEqual(unitsOf(report["units"]), want) {
		t.Fatalf("usage report = %v", report)
	}
	expectClose(t, ws, websocket.StatusNormalClosure)
	waitFor(t, "the cost record", func() bool { return len(h.costs.recorded()) == 1 })
	event := h.costs.recorded()[0]
	// 200*2 + 100*3 + 1*5 = 705: each measured unit at its own rate.
	if !event.Served || event.Source != providercost.SourceRateCard || !event.Complete || event.Cost.Amount != 705 {
		t.Errorf("cost event = %+v", event)
	}
}
