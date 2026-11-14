package realtime_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/realtime"
)

// The session's half of the live contract, against a scripted upstream: the
// provider side a native or emulated adapter implements. It returns
// delegation.requested from Next and receives the delegation commands through
// Send; the session numbers every event, supersedes and refuses. The adapters
// themselves are proven by TestLiveContract (live_contract_test.go).

// liveUpstream is a provider session the test drives: events it is told to
// emit, and every command the session sent it.
type liveUpstream struct {
	events chan provider.RealtimeUpstreamEvent
	// unserved are the commands it refuses with provider.ErrUnsupported,
	// as an upstream that cannot express them does.
	unserved map[contract.RealtimeCommandType]bool

	mu   sync.Mutex
	sent []contract.RealtimeCommand
}

func (u *liveUpstream) Send(_ context.Context, command contract.RealtimeCommand) error {
	if u.unserved[command.CommandType()] {
		return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "type", Detail: "this upstream cannot serve " + string(command.CommandType())}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sent = append(u.sent, command)
	return nil
}

func (u *liveUpstream) Next(ctx context.Context) (provider.RealtimeUpstreamEvent, error) {
	select {
	case event := <-u.events:
		return event, nil
	case <-ctx.Done():
		return provider.RealtimeUpstreamEvent{}, ctx.Err()
	}
}

func (u *liveUpstream) Close() error { return nil }

func (u *liveUpstream) emit(event contract.RealtimeServerEvent) {
	u.events <- provider.RealtimeUpstreamEvent{Event: event}
}

func (u *liveUpstream) received() []contract.RealtimeCommand {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]contract.RealtimeCommand(nil), u.sent...)
}

// liveOpener opens every session on the scripted upstream and settles nothing:
// this suite is about the session, not its settlement.
type liveOpener struct{ upstream *liveUpstream }

func (o liveOpener) OpenSession(context.Context, *contract.RealtimeSessionRequest) *kaana.SessionOpening {
	return &kaana.SessionOpening{Upstream: o.upstream, Route: provider.Route{
		DeploymentID: "dep_live", Provider: "openai-live", ModelReference: "openai/gpt-live-1@observed-2026-11-14",
	}}
}

func (liveOpener) SettleSession(context.Context, *kaana.SessionOpening, kaana.SessionEnd) kaana.SessionSettlement {
	return kaana.SessionSettlement{}
}

func newLiveHarness(t *testing.T, upstream *liveUpstream) *harness {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "edge-live"
	verifier, err := edgeauth.NewVerifier(map[string]ed25519.PublicKey{keyID: public}, 0)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := realtime.NewManager(realtime.Config{Opener: liveOpener{upstream: upstream}, Verifier: verifier,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /internal/v1/realtime", manager)
	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
		server.Close()
	})
	return &harness{t: t, server: server, manager: manager, private: private, keyID: keyID}
}

func liveRequest(requestID contract.RequestID) contract.RealtimeSessionRequest {
	request := sessionRequest(requestID, primary)
	request.Kind = contract.RealtimeLive
	request.Config = contract.RealtimeSessionConfig{
		Instructions: pointer("You are Alia."), Voice: pointer("marin"),
		InputAudioFormat: contract.RealtimePCM16, OutputAudioFormat: pointer(contract.RealtimePCM16),
		TurnDetection: contract.RealtimeTurnDetection{Type: contract.TurnDetectionServerVAD, CreateResponse: pointer(true), InterruptResponse: pointer(true)},
		Delegation:    &contract.RealtimeDelegationConfig{Acknowledgement: contract.RealtimeAcknowledgementAuto},
	}
	return request
}

func reply(requestID contract.RequestID, commandID contract.RealtimeCommandID, delegation contract.RealtimeDelegationID, final bool) *contract.RealtimeDelegationReplyCommand {
	return &contract.RealtimeDelegationReplyCommand{SchemaVersion: 1, RequestID: requestID, CommandID: commandID,
		DelegationID: delegation, Text: "Booked for 19:30.", Verbatim: []string{"19:30"}, Final: final}
}

// refusedFor requires the next frame to be a non-fatal invalid_request
// refusing commandID for param, sent without an acknowledgement.
func refusedFor(t *testing.T, ws *websocket.Conn, commandID, param string) {
	t.Helper()
	event := read(t, ws)
	failure, _ := event["error"].(map[string]any)
	if event.kind() != "error" || event["fatal"] != false || event["commandId"] != commandID ||
		failure["code"] != "invalid_request" || failure["param"] != param {
		t.Fatalf("%s: want a refusal naming %s, got %v", commandID, param, event)
	}
}

func accepted(t *testing.T, ws *websocket.Conn, commandID string, duplicate bool) {
	t.Helper()
	if event := read(t, ws); event.kind() != "command.accepted" || event["commandId"] != commandID || event["duplicate"] != duplicate {
		t.Fatalf("%s: want command.accepted (duplicate %v), got %v", commandID, duplicate, event)
	}
}

func commandTypes(commands []contract.RealtimeCommand) []string {
	kinds := make([]string, 0, len(commands))
	for _, command := range commands {
		kinds = append(kinds, string(command.CommandType()))
	}
	return kinds
}

// TestALiveSessionHandsItsTurnsOverAndRefusesWhatItsVoiceOwns runs the session
// rules of ADR 0012 §1: the commands a live session refuses, the delegation
// commands relayed for the open delegation only, supersession by a newer
// delegation, and the end of a delegation at its final reply or its cancel.
func TestALiveSessionHandsItsTurnsOverAndRefusesWhatItsVoiceOwns(t *testing.T) {
	upstream := &liveUpstream{events: make(chan provider.RealtimeUpstreamEvent)}
	h := newLiveHarness(t, upstream)
	const id = contract.RequestID("req_live")
	ws := h.open(liveRequest(id))
	if created := read(t, ws); created.kind() != "session.created" || created["kind"] != "live" {
		t.Fatalf("created = %v", created)
	}

	// What the voice owns is refused, unacknowledged; the audio it hears is not.
	command(t, ws, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_update",
		Config: contract.RealtimeSessionConfigUpdate{Instructions: pointer("shorter")}})
	refusedFor(t, ws, "cmd_update", "type")
	command(t, ws, &contract.RealtimeInputAudioCommitCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_commit"})
	refusedFor(t, ws, "cmd_commit", "type")
	command(t, ws, &contract.RealtimeInputAudioClearCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_clear"})
	refusedFor(t, ws, "cmd_clear", "type")
	command(t, ws, appendAudio(id, "cmd_audio", "AAAA"))
	accepted(t, ws, "cmd_audio", false)

	// No delegation is open yet.
	command(t, ws, reply(id, "cmd_early", "del_1", true))
	refusedFor(t, ws, "cmd_early", "delegationId")

	upstream.emit(&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_1", Text: "book a table"})
	if event := read(t, ws); event.kind() != "delegation.requested" || event["delegationId"] != "del_1" || event["text"] != "book a table" {
		t.Fatalf("requested = %v", event)
	}
	command(t, ws, reply(id, "cmd_part", "del_1", false))
	accepted(t, ws, "cmd_part", false)
	command(t, ws, &contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_context", DelegationID: "del_1", Text: "checking"})
	accepted(t, ws, "cmd_context", false)

	// A newer delegation before del_1's final reply supersedes it.
	upstream.emit(&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_2", Text: "no, tomorrow"})
	superseded, requested := read(t, ws), read(t, ws)
	if superseded.kind() != "delegation.superseded" || superseded["delegationId"] != "del_1" ||
		requested.kind() != "delegation.requested" || requested["delegationId"] != "del_2" {
		t.Fatalf("supersession = %v then %v", superseded, requested)
	}
	requireContiguous(t, []frame{superseded, requested}, superseded.sequence())
	command(t, ws, reply(id, "cmd_stale", "del_1", true))
	refusedFor(t, ws, "cmd_stale", "delegationId")

	// The final reply ends del_2: a repeat of it is a duplicate, anything new
	// naming it is refused.
	command(t, ws, reply(id, "cmd_final", "del_2", true))
	accepted(t, ws, "cmd_final", false)
	command(t, ws, reply(id, "cmd_final", "del_2", true))
	accepted(t, ws, "cmd_final", true)
	command(t, ws, &contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_after", DelegationID: "del_2", Text: "done"})
	refusedFor(t, ws, "cmd_after", "delegationId")

	// A cancel ends a delegation, and one ended that way is never superseded.
	upstream.emit(&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_3", Text: "and a taxi"})
	if event := read(t, ws); event.kind() != "delegation.requested" || event["delegationId"] != "del_3" {
		t.Fatalf("requested = %v", event)
	}
	command(t, ws, &contract.RealtimeDelegationCancelCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_cancel", DelegationID: "del_3"})
	accepted(t, ws, "cmd_cancel", false)
	command(t, ws, reply(id, "cmd_cancelled", "del_3", true))
	refusedFor(t, ws, "cmd_cancelled", "delegationId")

	// Only the session supersedes: one an adapter sends is not relayed.
	upstream.emit(&contract.RealtimeDelegationSupersededEvent{DelegationID: "del_3"})
	upstream.emit(&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_4", Text: "thanks"})
	if event := read(t, ws); event.kind() != "delegation.requested" || event["delegationId"] != "del_4" {
		t.Fatalf("after an adapter's superseded = %v", event)
	}

	command(t, ws, closeCommand(id, "cmd_close"))
	readUntil(t, ws, "session.closed")
	want := []string{"input_audio.append", "delegation.reply", "delegation.context", "delegation.reply", "delegation.cancel"}
	if got := commandTypes(upstream.received()); !reflect.DeepEqual(got, want) {
		t.Errorf("the upstream received %v, want %v: a refused or duplicate command must not reach it", got, want)
	}
}

// TestAnUpstreamThatCannotServeADelegationRefusesItAndTheSessionGoesOn: a
// delegation command reaches the upstream like any other, acknowledged first,
// and one it cannot express (provider.ErrUnsupported) is a non-fatal refusal
// that leaves the delegation open.
func TestAnUpstreamThatCannotServeADelegationRefusesItAndTheSessionGoesOn(t *testing.T) {
	upstream := &liveUpstream{events: make(chan provider.RealtimeUpstreamEvent),
		unserved: map[contract.RealtimeCommandType]bool{contract.RealtimeCommandDelegationContextType: true}}
	h := newLiveHarness(t, upstream)
	const id = contract.RequestID("req_live_unserved")
	ws := h.open(liveRequest(id))
	read(t, ws)
	upstream.emit(&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_1", Text: "hello"})
	read(t, ws)
	command(t, ws, &contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_context", DelegationID: "del_1", Text: "checking"})
	accepted(t, ws, "cmd_context", false)
	refusedFor(t, ws, "cmd_context", "type")
	command(t, ws, reply(id, "cmd_reply", "del_1", true))
	accepted(t, ws, "cmd_reply", false)
	command(t, ws, closeCommand(id, "cmd_close"))
	readUntil(t, ws, "session.closed")
	if got := commandTypes(upstream.received()); !reflect.DeepEqual(got, []string{"delegation.reply"}) {
		t.Errorf("the upstream received %v", got)
	}
}

// TestOnlyALiveSessionTakesDelegationCommands is the other half of the kind
// rule, on a conversation served by the OpenAI Realtime fake: a delegation
// command is refused before it is acknowledged and never reaches the provider,
// while the update a live session refuses is applied (the positive control).
func TestOnlyALiveSessionTakesDelegationCommands(t *testing.T) {
	h := newHarness(t, options{})
	const id = contract.RequestID("req_conversation_delegation")
	ws := h.open(sessionRequest(id, primary))
	read(t, ws)
	for _, refused := range []contract.RealtimeCommand{
		reply(id, "cmd_reply", "del_1", true),
		&contract.RealtimeDelegationContextCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_context", DelegationID: "del_1", Text: "x"},
		&contract.RealtimeDelegationCancelCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_cancel", DelegationID: "del_1"},
	} {
		command(t, ws, refused)
		_, commandID := refused.Identity()
		refusedFor(t, ws, string(commandID), "type")
	}
	command(t, ws, &contract.RealtimeSessionUpdateCommand{SchemaVersion: 1, RequestID: id, CommandID: "cmd_update",
		Config: contract.RealtimeSessionConfigUpdate{Instructions: pointer("shorter")}})
	accepted(t, ws, "cmd_update", false)
	command(t, ws, closeCommand(id, "cmd_close"))
	readUntil(t, ws, "session.closed")
	h.upstream.WaitClosed(t)
	if got := h.upstream.ReceivedTypes(); !reflect.DeepEqual(got, []string{"session.update", "session.update"}) {
		t.Errorf("the provider received %v; a refused delegation command must not reach it", got)
	}
}
