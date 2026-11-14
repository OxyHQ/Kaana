package realtime_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/realtime"
)

// TestLiveContract runs the whole canonical `live` contract (Kaana ADR 0012
// §1, §2) against every adapter that serves it, each on its own real-wire fake,
// through the whole data plane on the lane that serves `live`, kaana.ai/v1's
// `GET /v1/realtime` with its grant (liveGate): open, audio both ways, transcripts both ways, a
// delegation answered in two replies with a verbatim span, supersession,
// cancel, the refusals of the live kind, close and exactly-once settlement in
// the adapter's own units. A client never sees which adapter serves, so every
// one passes the same assertions; one that does not is not served for `live`.
//
// An adapter joins by adding its fixture to liveFixtures: one line here, and
// its liveBench and liveWire beside it (live_contract_openailive_test.go is
// the native GPT-Live one).
func TestLiveContract(t *testing.T) {
	for _, fixture := range liveFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			runLiveContract(t, fixture.start(t))
		})
	}
}

// liveFixtures are the adapters TestLiveContract proves.
var liveFixtures = []liveFixture{
	{name: "openai-live", start: openAILiveBench},
	{name: "openai-realtime", start: openAIRealtimeLiveBench},
	{name: "xai-realtime", start: xAIRealtimeLiveBench},
}

// liveFixture starts one live adapter on its fake.
type liveFixture struct {
	name  string
	start func(t *testing.T) liveBench
}

// liveBench is one started fixture: the adapter the data plane serves (with
// the keys `key_a` and `key_b` declared), and the provider its fake plays.
type liveBench struct {
	served served
	// wire returns the provider's side of the session the adapter opened.
	wire func(t *testing.T) liveWire
}

// liveWire is what a live provider does, in the contract's terms, each act
// spelled on that provider's own wire by its fake. The readings are snapshots
// of what reached the provider so far.
type liveWire interface {
	// Hear is the provider hearing the person say text: its input
	// transcript of it.
	Hear(text string)
	// Delegate is the voice handing the turn it heard over, by the
	// provider's own delegation or by the emulation's tool call; it returns
	// the delegation id the contract names.
	Delegate() contract.RealtimeDelegationID
	// Speak is the voice saying text aloud, with audio, as one utterance.
	Speak(text string, audio []byte)
	// AudioIn is the base64 audio the provider received, in order.
	AudioIn() []string
	// Spoken is what the provider was given to say for a delegation, as it
	// reached the wire (verbatim marks included); Context is the silent
	// context it was given for it; Cancelled is whether it was told the turn
	// was dropped.
	Spoken(id contract.RealtimeDelegationID) string
	Context(id contract.RealtimeDelegationID) string
	Cancelled(id contract.RealtimeDelegationID) bool
	// Settles is what the session must settle for what the provider did, once
	// it has closed: the units and their source.
	Settles() ([]contract.UsageQuantity, contract.UsageSource)
}

// liveGate admits the one session TestLiveContract opens, as Oxy's grant
// would, and keeps every settlement the session hands it.
type liveGate struct {
	request contract.RealtimeSessionRequest

	mu          sync.Mutex
	settlements []kaana.SessionSettlement
}

func (g *liveGate) Credential(r *http.Request) ([32]byte, bool) {
	value := r.Header.Get("Authorization")
	return sha256.Sum256([]byte(value)), value != ""
}

func (g *liveGate) OriginPatterns() []string { return nil }

func (g *liveGate) Admit(_ context.Context, r *http.Request, _ []byte) (*realtime.PublicAdmission, *contract.PublicError) {
	request := g.request
	digest, _ := g.Credential(r)
	return &realtime.PublicAdmission{Request: &request, Credential: digest,
		Settle: func(_ context.Context, settlement kaana.SessionSettlement, _ *kaana.SessionOpening) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.settlements = append(g.settlements, settlement)
		}}, nil
}

func (g *liveGate) settled() []kaana.SessionSettlement {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]kaana.SessionSettlement(nil), g.settlements...)
}

func liveContractRequest(requestID contract.RequestID, bench liveBench) contract.RealtimeSessionRequest {
	request := liveRequest(requestID)
	request.ModelReference = contract.ModelReference(bench.served.modelReference.ModelID())
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{Substitution: contract.SubstitutionSameModel, DeploymentID: primary,
		ModelReference: bench.served.modelReference, Provider: bench.served.slug, Regions: []contract.Region{}}}
	return request
}

// readUntilMatch reads frames up to and including the first that matches.
func readUntilMatch(t *testing.T, ws *websocket.Conn, what string, match func(frame) bool) []frame {
	t.Helper()
	var frames []frame
	for {
		next := read(t, ws)
		frames = append(frames, next)
		if match(next) {
			return frames
		}
		if next.kind() == "session.closed" {
			t.Fatalf("the session closed before %s: %s", what, describe(frames))
		}
	}
}

// transcriptOf is the text the deltas of one source carried, and its
// transcript.done, from frames.
func transcriptOf(frames []frame, source string) (deltas, done string, dones int) {
	for _, event := range frames {
		if event["source"] != source {
			continue
		}
		switch event.kind() {
		case "transcript.delta":
			text, _ := event["text"].(string)
			deltas += text
		case "transcript.done":
			done, _ = event["transcript"].(string)
			dones++
		}
	}
	return deltas, done, dones
}

func runLiveContract(t *testing.T, bench liveBench) {
	const id = contract.RequestID("req_live_contract")
	gate := &liveGate{request: liveContractRequest(id, bench)}
	h := serve(t, options{gate: gate}, bench.served)
	ws := publicConnect(t, h, "Bearer synthetic-live-contract", []byte(`{"type":"session.open"}`))
	ws.SetReadLimit(32 << 20)

	// Open.
	created := read(t, ws)
	if created.kind() != "session.created" || created["kind"] != "live" {
		t.Fatalf("session.created = %v", created)
	}
	wire := bench.wire(t)

	// What the voice owns is refused before it is acknowledged, and a reply
	// with no delegation open names delegationId.
	command(t, ws, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_update",
		Config: contract.RealtimeSessionConfigUpdate{Instructions: pointer("shorter")}})
	refusedFor(t, ws, "cmd_update", "type")
	command(t, ws, &contract.RealtimeInputAudioCommitCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_commit"})
	refusedFor(t, ws, "cmd_commit", "type")
	command(t, ws, &contract.RealtimeInputAudioClearCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_clear"})
	refusedFor(t, ws, "cmd_clear", "type")
	command(t, ws, reply(id, "cmd_early", "del_none", true))
	refusedFor(t, ws, "cmd_early", "delegationId")

	// Audio in: acknowledged, then on the provider's wire, exactly.
	heard := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4, 5, 6})
	command(t, ws, appendAudio(id, "cmd_audio", heard))
	accepted(t, ws, "cmd_audio", false)
	waitFor(t, "the audio to reach the provider", func() bool { return reflect.DeepEqual(wire.AudioIn(), []string{heard}) })

	// The person's words, relayed and handed over.
	wire.Hear("Book a table for two at half past seven.")
	first := wire.Delegate()
	turn := readUntilMatch(t, ws, "delegation.requested", func(f frame) bool { return f.kind() == "delegation.requested" })
	deltas, done, dones := transcriptOf(turn, "input_audio")
	requested := turn[len(turn)-1]
	if deltas != "Book a table for two at half past seven." || done != deltas || dones != 1 ||
		requested["delegationId"] != string(first) || requested["text"] != deltas {
		t.Fatalf("the turn handed over: %s; deltas %q, done %q (%d), requested %v", describe(turn), deltas, done, dones, requested)
	}

	// Answered in two replies, the second with a verbatim span, and context.
	partOne := "Let me check that for you."
	partTwo := "Your table for two is booked at 19:30, table 12."
	verbatim := []string{"19:30"}
	command(t, ws, &contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_context", DelegationID: first, Text: "Checking the bookings."})
	accepted(t, ws, "cmd_context", false)
	command(t, ws, &contract.RealtimeDelegationReplyCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_part_1", DelegationID: first, Text: partOne, Final: false})
	accepted(t, ws, "cmd_part_1", false)
	command(t, ws, &contract.RealtimeDelegationReplyCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_part_2", DelegationID: first, Text: partTwo, Verbatim: verbatim, Final: true})
	accepted(t, ws, "cmd_part_2", false)
	wrapped := provider.WrapVerbatim(partTwo, verbatim)
	waitFor(t, "the answer to reach the provider", func() bool {
		spoken := wire.Spoken(first)
		one, two := strings.Index(spoken, partOne), strings.Index(spoken, wrapped)
		return one >= 0 && two > one
	})
	if spoken := wire.Spoken(first); strings.Count(spoken, provider.VerbatimOpen+"19:30"+provider.VerbatimClose) != 1 ||
		strings.Count(spoken, provider.VerbatimOpen) != 1 {
		t.Fatalf("the voice was told %q: the verbatim span is wrapped once, and nothing else is", spoken)
	}
	if !strings.Contains(wire.Context(first), "Checking the bookings.") {
		t.Fatalf("the context the voice was given: %q", wire.Context(first))
	}
	// The final reply ended the delegation: anything more naming it is refused.
	command(t, ws, &contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_after_final", DelegationID: first, Text: "more"})
	refusedFor(t, ws, "cmd_after_final", "delegationId")

	// Audio and transcript out, the marks never relayed.
	spokenAudio := make([]byte, provider.MaxAudioChunkBytes+480)
	for index := range spokenAudio {
		spokenAudio[index] = byte(index % 251)
	}
	wire.Speak("Your table is at "+provider.VerbatimOpen+"19:30"+provider.VerbatimClose+".", spokenAudio)
	answer := readUntilMatch(t, ws, "the voice's transcript.done", func(f frame) bool {
		return f.kind() == "transcript.done" && f["source"] == "output_audio"
	})
	var out []byte
	for _, event := range answer {
		if event.kind() == "output_audio.delta" {
			data, _ := event["data"].(string)
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil || len(decoded) > provider.MaxAudioChunkBytes || event["format"] != string(contract.RealtimePCM16) {
				t.Fatalf("output audio frame = %v", event)
			}
			out = append(out, decoded...)
		}
		if event.kind() == "output_audio.done" {
			t.Fatalf("a live session sent output_audio.done: %v", event)
		}
	}
	deltas, done, dones = transcriptOf(answer, "output_audio")
	if !reflect.DeepEqual(out, spokenAudio) || deltas != "Your table is at 19:30." || done != deltas || dones != 1 {
		t.Fatalf("the voice: %d of %d audio bytes, deltas %q, done %q (%d): %s", len(out), len(spokenAudio), deltas, done, dones, describe(answer))
	}

	// Supersession: a newer turn before the open one's final reply.
	wire.Hear("And a taxi.")
	second := wire.Delegate()
	readUntilMatch(t, ws, "the second delegation", func(f frame) bool {
		return f.kind() == "delegation.requested" && f["delegationId"] == string(second)
	})
	wire.Hear("No, no taxi.")
	third := wire.Delegate()
	moved := readUntilMatch(t, ws, "the third delegation", func(f frame) bool {
		return f.kind() == "delegation.requested" && f["delegationId"] == string(third)
	})
	superseded := -1
	for index, event := range moved {
		if event.kind() == "delegation.superseded" {
			if event["delegationId"] != string(second) || superseded >= 0 {
				t.Fatalf("supersession: %s", describe(moved))
			}
			superseded = index
		}
	}
	if superseded != len(moved)-2 || moved[len(moved)-1]["text"] != "No, no taxi." ||
		moved[superseded+1].sequence() != moved[superseded].sequence()+1 {
		t.Fatalf("delegation.superseded must come right before the newer delegation.requested: %s", describe(moved))
	}
	command(t, ws, reply(id, "cmd_stale", second, true))
	refusedFor(t, ws, "cmd_stale", "delegationId")

	// Cancel: the provider is told, and the turn takes nothing more.
	command(t, ws, &contract.RealtimeDelegationCancelCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_cancel", DelegationID: third})
	accepted(t, ws, "cmd_cancel", false)
	waitFor(t, "the cancel to reach the provider", func() bool { return wire.Cancelled(third) })
	command(t, ws, reply(id, "cmd_cancelled", third, true))
	refusedFor(t, ws, "cmd_cancelled", "delegationId")
	if spoken := wire.Spoken(second) + wire.Spoken(third); spoken != "" {
		t.Fatalf("a refused reply reached the provider: %q", spoken)
	}

	// Close, and exactly-once settlement in the adapter's own units.
	command(t, ws, closeCommand(id, "cmd_close"))
	tail := readUntil(t, ws, "session.closed")
	closed := tail[len(tail)-1]
	units, source := wire.Settles()
	want := make(map[string]int, len(units))
	for _, quantity := range units {
		want[string(quantity.Unit)] = quantity.Quantity
	}
	if len(want) == 0 {
		t.Fatal("the fixture settles nothing: a completed session must carry a unit")
	}
	if closed["reason"] != "client_closed" || !reflect.DeepEqual(unitsOf(closed["units"]), want) || closed["usageSource"] != string(source) {
		t.Fatalf("session.closed = %v, want units %v from %s", closed, want, source)
	}
	// The public lane sends no usage report frame: the grant settles it.
	expectClose(t, ws, websocket.StatusNormalClosure)

	waitFor(t, "the settled session to be released", func() bool { return h.manager.Open() == 0 })
	time.Sleep(50 * time.Millisecond)
	settlements := gate.settled()
	if len(settlements) != 1 {
		t.Fatalf("%d settlements, want exactly one", len(settlements))
	}
	settlement := settlements[0]
	if report := settlement.Report; report == nil || report.RequestID != id || report.Outcome != contract.OutcomeCompleted ||
		report.DeploymentID != primary || report.ServingProvider != bench.served.slug || !reflect.DeepEqual(report.Units, units) || report.UsageSource != source {
		t.Fatalf("usage report = %+v, want %v from %s", settlement.Report, units, source)
	}
	// One attempt, priced in full by the card, recorded with the settlement
	// (the grant's outbox), not by the executor.
	cost := settlement.UpstreamCost
	if len(cost.Attempts) != 1 || !cost.Complete {
		t.Fatalf("upstream cost = %+v, want one priced attempt", cost)
	}
	if attempt := cost.Attempts[0]; attempt.KeyID != "key_a" || !attempt.Served || attempt.Provider != bench.served.slug ||
		attempt.Telemetry.Outcome != providercost.AttemptSucceeded || !reflect.DeepEqual(attempt.Units, units) ||
		attempt.Source != providercost.SourceRateCard {
		t.Fatalf("attempt = %+v, want the session's units %v priced by its card", attempt, units)
	}
	if recorded := h.costs.recorded(); len(recorded) != 0 {
		t.Fatalf("the executor recorded %d cost events for a granted session", len(recorded))
	}
	// No content reaches a log line.
	for _, content := range []string{"You are Alia.", "Book a table", "half past seven", "Checking the bookings", "table 12", heard} {
		if strings.Contains(h.logs.String(), content) {
			t.Fatalf("session content %q reached a log line", content)
		}
	}
}
