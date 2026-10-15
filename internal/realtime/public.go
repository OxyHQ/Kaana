package realtime

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/platformactivity"
)

// The public realtime surface: GET https://kaana.ai/v1/realtime (Kaana ADR
// 0002, decision 7).
//
// A public session lives in the same manager, with the same numbering, replay,
// limits and settlement as an internal one. What differs is at its edges: the
// caller authenticates with its own credential (Authorization or the
// kaana.bearer. subprotocol) and Oxy grants the session before it opens; every
// event is projected onto the public shapes (contract.PublicRealtimeEvent), so
// no provider or deployment is named; no usage report frame is sent, because
// the settlement goes to Oxy through the outbox instead; and a resume must
// present the credential that opened the session.

// MaxPublicFirstFrameBytes bounds a session.open: instructions and up to 128
// tool definitions.
const MaxPublicFirstFrameBytes = 1 << 20

// PublicSubprotocol is the subprotocol every public connection speaks.
const PublicSubprotocol = "kaana.v1"

// PublicAdmission is a public session Oxy granted.
type PublicAdmission struct {
	// Request is the session, resolved from the grant into signed routes.
	Request *contract.RealtimeSessionRequest
	// Credential is the digest of the credential that opened it; a resume
	// must present the same one.
	Credential [32]byte
	// Settle receives the session's settlement exactly once, when it ends,
	// for the outbox. Its cost was not recorded by the executor.
	Settle func(ctx context.Context, settlement kaana.SessionSettlement, opening *kaana.SessionOpening)
}

// PublicGate admits public sessions: the caller's credential, the grant and
// the routes. internal/publicapi implements it.
type PublicGate interface {
	// Admit reads the session.open frame and asks Oxy for the session's grant.
	// A refusal is sent to the caller as one fatal public error event.
	Admit(ctx context.Context, r *http.Request, frame []byte) (*PublicAdmission, *contract.PublicError)
	// Credential is the digest of the credential a connection presents.
	Credential(r *http.Request) ([32]byte, bool)
	// OriginPatterns are the browser origins (hosts) allowed to connect.
	OriginPatterns() []string
}

// PublicHandler serves the public realtime endpoint through gate.
func (m *Manager) PublicHandler(gate PublicGate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		draining := m.draining
		m.mu.Unlock()
		if draining {
			http.Error(w, "kaana is draining", http.StatusServiceUnavailable)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:   []string{PublicSubprotocol},
			OriginPatterns: gate.OriginPatterns(),
		})
		if err != nil {
			return
		}
		ws.SetReadLimit(MaxPublicFirstFrameBytes)
		readContext, cancel := context.WithTimeout(r.Context(), FirstFrameTimeout)
		kind, frame, err := ws.Read(readContext)
		cancel()
		if err != nil || kind != websocket.MessageText {
			_ = ws.Close(websocket.StatusPolicyViolation, "the first frame is session.open or session.resume")
			return
		}
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(frame, &probe)
		if probe.Type == string(contract.RealtimeCommandSessionResumeType) {
			m.publicResume(r, ws, frame, gate)
			return
		}
		admission, failure := gate.Admit(r.Context(), r, frame)
		if failure != nil {
			m.refusePublic(r.Context(), ws, failure, nil)
			return
		}
		platformactivity.MarkVerified(r.Context())
		request := admission.Request
		sessionContext, stop := context.WithCancel(context.WithoutCancel(r.Context()))
		s := newSession(stop, m, request)
		s.public = true
		s.credential = admission.Credential
		s.settlePublic = admission.Settle
		m.mu.Lock()
		if _, live := m.sessions[request.Attribution.RequestID]; live || m.draining {
			m.mu.Unlock()
			stop()
			m.refusePublic(r.Context(), ws, contract.NewPublicError(request.Attribution.RequestID, "service_unavailable", contract.PublicMessage("service_unavailable")), nil)
			return
		}
		m.sessions[request.Attribution.RequestID] = s
		m.running.Add(1)
		m.mu.Unlock()
		go s.run(sessionContext, ws)
	})
}

// publicResume attaches a reconnection to the public session it names, only
// for the credential that opened it. A session that does not exist, ended,
// is internal or belongs to another credential is refused the same way, so a
// resume cannot probe which sessions exist.
func (m *Manager) publicResume(r *http.Request, ws *websocket.Conn, frame []byte, gate PublicGate) {
	var probe struct {
		RequestID contract.RequestID `json:"requestId"`
	}
	_ = json.Unmarshal(frame, &probe)
	decoded, err := contract.DecodeRealtimeCommand(frame, probe.RequestID)
	command, isResume := decoded.(*contract.RealtimeSessionResumeCommand)
	if err != nil || !isResume {
		m.refusePublic(r.Context(), ws, contract.NewPublicError(probe.RequestID, "invalid_request", "the session.resume frame is invalid"), nil)
		return
	}
	commandID := command.CommandID
	refuse := func(ctx context.Context) {
		m.refusePublic(ctx, ws, contract.NewPublicError(command.RequestID, "invalid_request",
			"no resumable session with this requestId is open for this credential on this server").WithParam("requestId"), &commandID)
	}
	digest, presented := gate.Credential(r)
	m.mu.Lock()
	s := m.sessions[command.RequestID]
	m.mu.Unlock()
	if s == nil || !s.public || !presented || subtle.ConstantTimeCompare(digest[:], s.credential[:]) != 1 {
		refuse(r.Context())
		return
	}
	reply := make(chan error, 1)
	select {
	case s.attach <- attachment{ws: ws, command: command, reply: reply}:
	case <-s.done:
		refuse(r.Context())
		return
	}
	select {
	case err := <-reply:
		if err != nil {
			m.refusePublic(r.Context(), ws, contract.NewPublicError(command.RequestID, "invalid_request", err.Error()).WithParam("afterSequence"), &commandID)
		}
	case <-s.done:
		refuse(r.Context())
	}
}

// refusePublic answers a connection that never attached with one fatal public
// error event, outside any session's sequence (sequence 0), and a policy
// close.
func (m *Manager) refusePublic(ctx context.Context, ws *websocket.Conn, failure *contract.PublicError, commandID *contract.RealtimeCommandID) {
	event := &contract.RealtimePublicErrorEvent{Fatal: true, Error: *failure}
	event.Stamp(contract.RealtimeEventHeader{SchemaVersion: contract.RealtimeSchemaVersion, RequestID: failure.RequestID, Sequence: 0, CommandID: commandID})
	if encoded, err := json.Marshal(event); err == nil {
		writeContext, cancel := context.WithTimeout(ctx, m.writeTimeout)
		_ = ws.Write(writeContext, websocket.MessageText, encoded)
		cancel()
	}
	_ = ws.Close(websocket.StatusPolicyViolation, "")
}
