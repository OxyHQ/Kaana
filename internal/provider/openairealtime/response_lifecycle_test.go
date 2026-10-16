package openairealtime

import (
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// TestEveryResponseCreatedGetsItsResponseDone: xAI cancels an answer the caller
// talks over and opens the next response with no response.done for the first
// (measured 2026-10-16 against grok-voice-3.0). The client is still owed one,
// in order: the first is reported done, cancelled, before the second is created.
// A response that does end with its own response.done is the control.
func TestEveryResponseCreatedGetsItsResponseDone(t *testing.T) {
	upstream := fake.NewXAI(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_talked_over", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_answer", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_answer", "object": "realtime.response", "status": "completed", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_next", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
	}
	session := xaiOpen(t, upstream, pushToTalk())

	type step struct {
		created contract.RealtimeResponseID
		done    contract.RealtimeResponseID
		status  contract.RealtimeResponseStatus
	}
	var got []step
	for len(got) < 5 {
		switch event := next(t, session).Event.(type) {
		case *contract.RealtimeResponseCreatedEvent:
			got = append(got, step{created: event.ResponseID})
		case *contract.RealtimeResponseDoneEvent:
			got = append(got, step{done: event.ResponseID, status: event.Status})
			if event.ResponseID == "resp_talked_over" && (event.FinishReason == nil || *event.FinishReason != contract.FinishCancelled) {
				t.Errorf("the talked-over response finished %v, want cancelled", event.FinishReason)
			}
		}
	}
	want := []step{
		{created: "resp_talked_over"},
		{done: "resp_talked_over", status: contract.RealtimeResponseCancelled},
		{created: "resp_answer"},
		{done: "resp_answer", status: contract.RealtimeResponseCompleted},
		{created: "resp_next"},
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("response lifecycle = %+v\nwant %+v", got, want)
		}
	}
}
