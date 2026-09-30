// Package realtime serves realtime sessions on GET /internal/v1/realtime: the
// WebSocket the Oxy edge opens to Kaana for one signed session.
//
// The wire between the edge and Kaana is fixed by docs/realtime.md. In
// outline: the upgrade carries the edge signature headers; the first text
// frame is the signed session request (or a signed session.resume), verified
// over its exact bytes; every later frame is one JSON command in, one JSON
// event out; every event carries the session's one monotonic sequence; each
// command is acknowledged before it is applied and applied at most once; and
// the session settles exactly once, with session.closed followed by one usage
// report frame.
//
// Routing and settlement are the executor's (kaana.Executor.OpenSession and
// SettleSession). This package owns the session's life in between: numbering,
// the replay buffer a reconnect reads, command deduplication, the signed
// limits, the idle and duration timers, and which connection is attached.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/platformactivity"
)

// Bounds fixed by the edge<->Kaana wire (docs/realtime.md).
const (
	// MaxFirstFrameBytes bounds the signed first frame.
	MaxFirstFrameBytes = 64 << 10
	// FirstFrameTimeout is how long a connection may take to send it.
	FirstFrameTimeout = 10 * time.Second
	// DefaultResumeWindow is how long a dropped connection may be resumed
	// after, as session.created states it.
	DefaultResumeWindow = 30 * time.Second
)

// Bounds this build chooses.
const (
	// maxCommandBytes bounds one command frame after the first. The largest
	// legal command is a conversation item: up to 64 parts, each an audio frame
	// or up to a million characters of text.
	maxCommandBytes = 16 << 20
	// defaultReplayEvents and defaultReplayBytes bound the replay buffer. A
	// resume window of speech at the densest format is about 2 MiB of base64
	// per 30 seconds, well inside.
	defaultReplayEvents = 8192
	defaultReplayBytes  = 16 << 20
	// defaultWriteTimeout bounds one write to the edge. A write that cannot
	// complete in it is a connection that is gone.
	defaultWriteTimeout = 10 * time.Second
	// defaultPingInterval keeps a quiet connection inside the idle timeout of
	// the load balancer in front of Kaana (60 seconds by default on an AWS
	// ALB) with room to spare.
	defaultPingInterval = 20 * time.Second
)

// Opener is the executor's half of a session: routing it open and settling it.
type Opener interface {
	OpenSession(ctx context.Context, request *contract.RealtimeSessionRequest) *kaana.SessionOpening
	SettleSession(ctx context.Context, opening *kaana.SessionOpening, end kaana.SessionEnd) kaana.SessionSettlement
}

// Config wires a Manager.
type Config struct {
	Opener   Opener
	Verifier *edgeauth.Verifier
	Logger   *slog.Logger
	// ResumeWindow is how long a dropped connection may be resumed after.
	// Zero takes DefaultResumeWindow; the contract caps it at one minute.
	ResumeWindow time.Duration
	// ReplayEvents and ReplayBytes bound each session's replay buffer. Zero
	// takes the defaults.
	ReplayEvents int
	ReplayBytes  int
	// WriteTimeout bounds one write to the edge. Zero takes the default.
	WriteTimeout time.Duration
	// PingInterval is how often a quiet attached connection is pinged. Zero
	// takes the default.
	PingInterval time.Duration
	// Now is the clock for timestamps, injectable for tests.
	Now func() time.Time
}

// Manager holds every session this task has open. Sessions live in its
// memory: a resume that reaches another task does not find them (by design,
// docs/realtime.md).
type Manager struct {
	opener       Opener
	verifier     *edgeauth.Verifier
	logger       *slog.Logger
	resumeWindow time.Duration
	replayEvents int
	replayBytes  int
	writeTimeout time.Duration
	pingInterval time.Duration
	now          func() time.Time

	shutdown chan struct{}

	mu       sync.Mutex
	sessions map[contract.RequestID]*session
	draining bool
	running  sync.WaitGroup
}

// NewManager builds the session manager.
func NewManager(config Config) (*Manager, error) {
	switch {
	case config.Opener == nil:
		return nil, errors.New("realtime: no session opener")
	case config.Verifier == nil:
		return nil, errors.New("realtime: no edge signature verifier")
	case config.ResumeWindow < 0 || config.ResumeWindow > contract.MaxRealtimeResumeWindowMs*time.Millisecond:
		return nil, fmt.Errorf("realtime: the resume window is at most %d ms", contract.MaxRealtimeResumeWindowMs)
	}
	manager := &Manager{
		opener: config.Opener, verifier: config.Verifier, logger: config.Logger,
		resumeWindow: config.ResumeWindow, replayEvents: config.ReplayEvents, replayBytes: config.ReplayBytes,
		writeTimeout: config.WriteTimeout, pingInterval: config.PingInterval, now: config.Now,
		shutdown: make(chan struct{}), sessions: make(map[contract.RequestID]*session),
	}
	if manager.logger == nil {
		manager.logger = slog.Default()
	}
	if manager.resumeWindow == 0 {
		manager.resumeWindow = DefaultResumeWindow
	}
	if manager.replayEvents <= 0 {
		manager.replayEvents = defaultReplayEvents
	}
	if manager.replayBytes <= 0 {
		manager.replayBytes = defaultReplayBytes
	}
	if manager.writeTimeout <= 0 {
		manager.writeTimeout = defaultWriteTimeout
	}
	if manager.pingInterval <= 0 {
		manager.pingInterval = defaultPingInterval
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	return manager, nil
}

// ServeHTTP upgrades one connection and reads its signed first frame.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	draining := m.draining
	m.mu.Unlock()
	if draining {
		http.Error(w, "kaana is draining", http.StatusServiceUnavailable)
		return
	}
	// The signature rides on the upgrade and covers the first frame, so the
	// headers are kept from the request that is about to be hijacked.
	header := r.Header.Clone()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Authentication is the signed first frame, never an ambient browser
		// credential, so an Origin check would protect nothing here and would
		// refuse a server-side client that happens to send one.
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept has already answered the failed upgrade.
		return
	}
	ws.SetReadLimit(MaxFirstFrameBytes)
	readContext, cancel := context.WithTimeout(r.Context(), FirstFrameTimeout)
	kind, frame, err := ws.Read(readContext)
	cancel()
	if err != nil || kind != websocket.MessageText {
		_ = ws.Close(websocket.StatusPolicyViolation, "")
		return
	}
	if err := m.verifier.Verify(header, frame); err != nil {
		// Nothing about the near-miss is logged: the headers and the frame are
		// attacker-controlled text.
		m.logger.Warn("rejected an unsigned or badly signed realtime connection", "path", r.URL.Path)
		_ = ws.Close(websocket.StatusPolicyViolation, "")
		return
	}
	platformactivity.MarkVerified(r.Context())

	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &probe) != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "")
		return
	}
	if probe.Type == string(contract.RealtimeCommandSessionResumeType) {
		m.resume(r.Context(), ws, frame)
		return
	}
	m.open(r.Context(), ws, frame)
}

// open starts a session from a signed session request. Unknown fields are
// tolerated as for every inbound envelope; a frame that is not a valid session
// request is refused whole before anything is routed.
//
// The session's context keeps the request's values but not its cancellation:
// a session outlives the connection that opened it. It ends with the session,
// or when the manager is stopped outright.
func (m *Manager) open(ctx context.Context, ws *websocket.Conn, frame []byte) {
	var version struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	var request contract.RealtimeSessionRequest
	if json.Unmarshal(frame, &version) != nil || version.SchemaVersion == nil || *version.SchemaVersion != contract.RealtimeSchemaVersion ||
		json.Unmarshal(frame, &request) != nil || request.Validate() != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, "invalid realtime session request")
		return
	}
	requestID := request.Attribution.RequestID
	sessionContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := newSession(cancel, m, &request)
	m.mu.Lock()
	if _, live := m.sessions[requestID]; live || m.draining {
		m.mu.Unlock()
		cancel()
		_ = ws.Close(websocket.StatusPolicyViolation, "the session is already open")
		return
	}
	m.sessions[requestID] = s
	m.running.Add(1)
	m.mu.Unlock()
	go s.run(sessionContext, ws)
}

// resume hands a signed session.resume to the session it names, which
// replays what the client missed and attaches the connection. A resume the
// session cannot honour is refused on this connection only.
func (m *Manager) resume(ctx context.Context, ws *websocket.Conn, frame []byte) {
	var probe struct {
		RequestID contract.RequestID `json:"requestId"`
	}
	_ = json.Unmarshal(frame, &probe)
	decoded, err := contract.DecodeRealtimeCommand(frame, probe.RequestID)
	command, isResume := decoded.(*contract.RealtimeSessionResumeCommand)
	if err != nil || !isResume {
		_ = ws.Close(websocket.StatusPolicyViolation, "")
		return
	}
	m.mu.Lock()
	s := m.sessions[command.RequestID]
	m.mu.Unlock()
	if s == nil {
		m.refuseResume(ctx, ws, command, "no session with this requestId is open on this task")
		return
	}
	reply := make(chan error, 1)
	select {
	case s.attach <- attachment{ws: ws, command: command, reply: reply}:
	case <-s.done:
		m.refuseResume(ctx, ws, command, "the session has ended")
		return
	}
	select {
	case err := <-reply:
		if err != nil {
			m.refuseResume(ctx, ws, command, err.Error())
		}
	case <-s.done:
		m.refuseResume(ctx, ws, command, "the session has ended")
	}
}

// refuseResume answers a resume that cannot be honoured with a fatal
// invalid_request error and a policy close. The error stands outside the
// session's sequence (it is sent on a connection that never attached), so it
// carries sequence 0 and consumes no number.
func (m *Manager) refuseResume(ctx context.Context, ws *websocket.Conn, command *contract.RealtimeSessionResumeCommand, detail string) {
	commandID := command.CommandID
	refusal := &contract.RealtimeErrorEvent{Fatal: true, Error: *contract.NewError(command.RequestID, contract.CodeInvalidRequest, detail)}
	refusal.Stamp(contract.RealtimeEventHeader{SchemaVersion: contract.RealtimeSchemaVersion, RequestID: command.RequestID, Sequence: 0, CommandID: &commandID})
	if encoded, err := json.Marshal(refusal); err == nil {
		writeContext, cancel := context.WithTimeout(ctx, m.writeTimeout)
		_ = ws.Write(writeContext, websocket.MessageText, encoded)
		cancel()
	}
	_ = ws.Close(websocket.StatusPolicyViolation, "")
}

func (m *Manager) remove(s *session) {
	m.mu.Lock()
	if m.sessions[s.requestID] == s {
		delete(m.sessions, s.requestID)
	}
	m.mu.Unlock()
}

// Open reports how many sessions this task holds.
func (m *Manager) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Shutdown closes every session with server_shutdown and waits for each to
// settle. New connections are refused from the moment it is called. If ctx
// ends first, the sessions still running are cancelled: an open in progress is
// abandoned, and each settles without waiting on its client.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.draining {
		m.draining = true
		close(m.shutdown)
	}
	m.mu.Unlock()
	settled := make(chan struct{})
	go func() {
		m.running.Wait()
		close(settled)
	}()
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		for _, s := range m.sessions {
			s.cancel()
		}
		m.mu.Unlock()
		<-settled
		return fmt.Errorf("realtime: sessions were still open when the drain ended: %w", ctx.Err())
	}
}
