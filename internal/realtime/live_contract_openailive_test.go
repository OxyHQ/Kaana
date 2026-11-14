package realtime_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openailive"
	livefake "github.com/OxyHQ/Kaana/internal/provider/openailive/openailivetest"
)

// openAILiveBench is the native GPT-Live adapter on its fake, for
// TestLiveContract. GPT-Live bills the session's seconds, which its
// session.closed reports: the fake bills 42.0001 s, settled as 42 001 ms,
// provider_reported, at OpenAI's $0.05 a minute (833 334 × 10⁻¹² USD per ms).
func openAILiveBench(t *testing.T) liveBench {
	upstream := livefake.New(t)
	adapter, err := openailive.New(openailive.Config{
		Declarations: []provider.KeyDeclaration{{KeyID: "key_a", Secret: livefake.Key}, {KeyID: "key_b", Secret: livefake.SecondKey}},
		HTTPClient:   upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return liveBench{
		served: served{adapter: adapter, slug: openailive.Slug, upstreamModel: "gpt-live-1",
			modelReference: "openai/gpt-live-1@observed-2026-11-14", rates: `{"unit":"session_milliseconds","amountPerUnit":833334}`},
		wire: func(t *testing.T) liveWire {
			conn := upstream.Accept(t)
			conn.Bill("42.0001")
			return &openAILiveWire{upstream: upstream, conn: conn}
		},
	}
}

// openAILiveWire plays GPT-Live: transcript fragments on its session clock,
// client delegations, and the appends Kaana answers them with.
type openAILiveWire struct {
	upstream *livefake.Upstream
	conn     *livefake.Conn

	mu          sync.Mutex
	clockMs     int64
	delegations int
}

// advance moves GPT-Live's session clock and returns the span it covered.
func (w *openAILiveWire) advance(ms int64) (start, end int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	start = w.clockMs
	w.clockMs += ms
	return start, w.clockMs
}

func (w *openAILiveWire) Hear(text string) {
	// Two fragments, as GPT-Live streams a transcript.
	cut := strings.LastIndex(text, " ")
	start, middle := w.advance(400)
	w.conn.Hear(text[:cut], start, middle)
	_, end := w.advance(400)
	w.conn.Hear(text[cut:], middle, end)
}

func (w *openAILiveWire) Delegate() contract.RealtimeDelegationID {
	_, offset := w.advance(200)
	w.mu.Lock()
	w.delegations++
	id := fmt.Sprintf("dlg_live_%d", w.delegations)
	w.mu.Unlock()
	w.conn.Delegate(id, offset)
	return contract.RealtimeDelegationID(id)
}

func (w *openAILiveWire) Speak(text string, audio []byte) {
	// The utterance ends when the voice falls silent: GPT-Live sends no done
	// event, so this one is finished by the adapter's silence gap.
	w.conn.Speak(audio)
	cut := strings.Index(text, " ")
	start, middle := w.advance(300)
	w.conn.Say(text[:cut], start, middle)
	_, end := w.advance(600)
	w.conn.Say(text[cut:], middle, end)
	w.advance(5_000)
}

func (w *openAILiveWire) received(eventType string) []livefake.Event {
	var events []livefake.Event
	for _, event := range w.upstream.Received() {
		if event.Type() == eventType {
			events = append(events, event)
		}
	}
	return events
}

func (w *openAILiveWire) AudioIn() []string {
	var audio []string
	for _, event := range w.received("session.input_audio.append") {
		data, _ := event["audio"].(string)
		audio = append(audio, data)
	}
	return audio
}

// contents is the content of every append of one type for a delegation, in
// order, joined as the voice reads them.
func (w *openAILiveWire) contents(eventType string, id contract.RealtimeDelegationID) string {
	var parts []string
	for _, event := range w.received(eventType) {
		if event["delegation_id"] == string(id) {
			content, _ := event["content"].(string)
			if content != "" {
				parts = append(parts, content)
			}
		}
	}
	return strings.Join(parts, " ")
}

func (w *openAILiveWire) Spoken(id contract.RealtimeDelegationID) string {
	return w.contents("session.commentary.append", id)
}

func (w *openAILiveWire) Context(id contract.RealtimeDelegationID) string {
	return w.contents("session.thinking.append", id)
}

func (w *openAILiveWire) Cancelled(id contract.RealtimeDelegationID) bool {
	return w.contents("session.instructions.append", id) != ""
}

func (w *openAILiveWire) Settles() ([]contract.UsageQuantity, contract.UsageSource) {
	return []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 42_001}}, contract.UsageProviderReported
}

// TestAGPTLiveSessionClosedRightAfterTheVoiceStillEndsItsUtterance: GPT-Live
// has no event that ends what the voice says, so an utterance the caller
// closes on before its silence gap is ended at settlement: its transcript.done
// precedes session.closed. Without it the call's last line would be lost.
func TestAGPTLiveSessionClosedRightAfterTheVoiceStillEndsItsUtterance(t *testing.T) {
	bench := openAILiveBench(t)
	const id = contract.RequestID("req_live_trailing")
	gate := &liveGate{request: liveContractRequest(id, bench)}
	h := serve(t, options{gate: gate}, bench.served)
	ws := publicConnect(t, h, "Bearer synthetic-live-trailing", []byte(`{"type":"session.open"}`))
	if created := read(t, ws); created.kind() != "session.created" {
		t.Fatalf("session.created = %v", created)
	}
	wire := bench.wire(t).(*openAILiveWire)
	start, end := wire.advance(800)
	wire.conn.Say("Goodbye.", start, end)
	readUntilMatch(t, ws, "the voice's words", func(f frame) bool { return f.kind() == "transcript.delta" && f["source"] == "output_audio" })
	command(t, ws, closeCommand(id, "cmd_close"))
	tail := readUntil(t, ws, "session.closed")
	_, done, dones := transcriptOf(tail, "output_audio")
	if done != "Goodbye." || dones != 1 || tail[len(tail)-2].kind() != "transcript.done" {
		t.Fatalf("the voice's last utterance was not ended before session.closed: %s", describe(tail))
	}
}
