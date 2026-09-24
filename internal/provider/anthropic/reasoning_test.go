package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type quietEmitter struct{}

func (quietEmitter) Start(contract.ModelReference, time.Time) error             { return nil }
func (quietEmitter) Delta(int, contract.DeltaChannel, string) error             { return nil }
func (quietEmitter) ToolCall(provider.ToolCallDelta) error                      { return nil }
func (quietEmitter) Usage([]contract.UsageQuantity, contract.UsageSource) error { return nil }

// sendEffort drives Translate and Stream against a fake speaking the real
// Messages wire and returns the exact body the upstream received.
func sendEffort(t *testing.T, effort contract.ReasoningEffort) map[string]json.RawMessage {
	t.Helper()
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_fake","type":"message","role":"assistant","model":"claude-fake",
		  "content":[{"type":"thinking","thinking":"","signature":"c2ln"},{"type":"text","text":"ok"}],
		  "stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":5}}`))
	}))
	t.Cleanup(upstream.Close)

	adapter, err := New(Config{BaseURL: upstream.URL + "/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	request := baseRequest("req_effort")
	request.Stream = false
	if effort != "" {
		request.Reasoning = &contract.ReasoningParameters{Effort: effort}
	}
	call, err := adapter.Translate(request, testRoute())
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if _, err := adapter.Stream(context.Background(), call, quietEmitter{}, nil); err != nil {
		t.Fatalf("streaming: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(<-received, &wire); err != nil {
		t.Fatalf("the upstream body is not JSON: %v", err)
	}
	return wire
}

func TestReasoningEffortReachesTheMessagesAPIAsOutputConfigEffort(t *testing.T) {
	for _, effort := range contract.ReasoningEfforts() {
		wire := sendEffort(t, effort)
		if string(wire["output_config"]) != `{"effort":"`+string(effort)+`"}` {
			t.Errorf("%s: output_config on the wire = %s", effort, wire["output_config"])
		}
		// No budget is invented from the effort, and max_tokens stays the
		// caller's own.
		if _, invented := wire["thinking"]; invented {
			t.Errorf("%s: a thinking budget was synthesised: %s", effort, wire["thinking"])
		}
		if string(wire["max_tokens"]) != "256" {
			t.Errorf("%s: max_tokens moved to %s", effort, wire["max_tokens"])
		}
	}
}

func TestAnAbsentEffortSendsNoOutputConfig(t *testing.T) {
	wire := sendEffort(t, "")
	if _, present := wire["output_config"]; present {
		t.Errorf("output_config was invented for a request that named no effort: %s", wire["output_config"])
	}
}
