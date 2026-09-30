package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// inbound is one frame, or the end, of one attached connection.
type inbound struct {
	conn *connection
	kind websocket.MessageType
	data []byte
	err  error
}

// upstreamResult is one normalized event, or the end, of the provider session.
type upstreamResult struct {
	event provider.RealtimeUpstreamEvent
	err   error
}

// attachment is a verified resume asking for its connection to be attached.
type attachment struct {
	ws      *websocket.Conn
	command *contract.RealtimeSessionResumeCommand
	reply   chan error
}

// connection is one attached WebSocket and its reader.
type connection struct {
	ws     *websocket.Conn
	cancel context.CancelFunc
}

// session is one realtime session. Everything below `done` is owned by the
// run goroutine and touched nowhere else: one goroutine numbers the events, so
// no two can share a sequence.
type session struct {
	manager   *Manager
	requestID contract.RequestID
	request   *contract.RealtimeSessionRequest

	cancel context.CancelFunc
	inbox  chan inbound
	events chan upstreamResult
	attach chan attachment
	done   chan struct{}

	opening       *kaana.SessionOpening
	sequence      int
	replay        *replay
	seen          map[contract.RealtimeCommandID]struct{}
	attached      *connection
	units         map[contract.UsageUnit]int
	inputBytes    int
	outputBytes   int
	responses     int
	firstOutputAt time.Time
	idle          *time.Timer
	expiry        *time.Timer
	resumeTimer   *time.Timer
	closed        bool
}

func newSession(cancel context.CancelFunc, manager *Manager, request *contract.RealtimeSessionRequest) *session {
	return &session{
		manager: manager, requestID: request.Attribution.RequestID, request: request,
		cancel: cancel,
		inbox:  make(chan inbound, 64), events: make(chan upstreamResult), attach: make(chan attachment), done: make(chan struct{}),
		replay: newReplay(manager.replayEvents, manager.replayBytes),
		seen:   make(map[contract.RealtimeCommandID]struct{}), units: make(map[contract.UsageUnit]int),
	}
}

// stoppedTimer is a timer that has not been armed: its channel never fires.
func stoppedTimer() *time.Timer {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return timer
}

func (s *session) run(ctx context.Context, first *websocket.Conn) {
	defer func() {
		s.cancel()
		s.manager.remove(s)
		close(s.done)
		s.manager.running.Done()
	}()
	s.idle, s.expiry, s.resumeTimer = stoppedTimer(), stoppedTimer(), stoppedTimer()
	s.connect(ctx, first)

	s.opening = s.manager.opener.OpenSession(ctx, s.request)
	if !s.opening.Opened() {
		reason := contract.RealtimeNoRouteAvailable
		if s.shuttingDown(ctx) {
			reason = contract.RealtimeServerShutdown
		}
		s.finish(ctx, reason, s.opening.Failure, nil, websocket.StatusNormalClosure)
		return
	}
	go s.pump(ctx)

	openedAt := s.manager.now()
	limits := s.request.Limits
	route := s.opening.Route
	s.emit(ctx, &contract.RealtimeSessionCreatedEvent{
		ResolvedModelReference: route.ModelReference, ServingProvider: route.Provider, DeploymentID: route.DeploymentID,
		Kind: s.request.Kind, Config: s.request.Config, Limits: limits,
		ResumeWindowMs: int(s.manager.resumeWindow.Milliseconds()),
		StartedAt:      contract.NewTimestamp(openedAt),
		ExpiresAt:      contract.NewTimestamp(openedAt.Add(time.Duration(limits.MaxDurationMs) * time.Millisecond)),
	}, nil)
	s.expiry.Reset(time.Duration(limits.MaxDurationMs) * time.Millisecond)
	s.idle.Reset(time.Duration(limits.IdleTimeoutMs) * time.Millisecond)

	for !s.closed {
		select {
		case frame := <-s.inbox:
			s.receive(ctx, frame)
		case result := <-s.events:
			s.upstream(ctx, result)
		case request := <-s.attach:
			request.reply <- s.resume(ctx, request)
		case <-s.idle.C:
			s.finish(ctx, contract.RealtimeIdleTimeout, nil, nil, websocket.StatusNormalClosure)
		case <-s.expiry.C:
			s.finish(ctx, contract.RealtimeMaxDuration, nil, nil, websocket.StatusNormalClosure)
		case <-s.resumeTimer.C:
			s.finish(ctx, contract.RealtimeResumeExpired, nil, nil, websocket.StatusNormalClosure)
		case <-s.manager.shutdown:
			s.finish(ctx, contract.RealtimeServerShutdown, nil, nil, websocket.StatusNormalClosure)
		case <-ctx.Done():
			s.finish(ctx, contract.RealtimeServerShutdown, nil, nil, websocket.StatusNormalClosure)
		}
	}
}

func (s *session) shuttingDown(ctx context.Context) bool {
	select {
	case <-s.manager.shutdown:
		return true
	default:
		return ctx.Err() != nil
	}
}

// pump reads the provider session until it ends. It is the only caller of
// Next.
func (s *session) pump(ctx context.Context) {
	for {
		event, err := s.opening.Upstream.Next(ctx)
		select {
		case s.events <- upstreamResult{event: event, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// connect attaches a connection and starts reading it.
func (s *session) connect(sessionContext context.Context, ws *websocket.Conn) {
	ws.SetReadLimit(maxCommandBytes)
	ctx, cancel := context.WithCancel(sessionContext)
	conn := &connection{ws: ws, cancel: cancel}
	s.attached = conn
	s.resumeTimer.Stop()
	go func() {
		for {
			kind, data, err := ws.Read(ctx)
			select {
			case s.inbox <- inbound{conn: conn, kind: kind, data: data, err: err}:
			case <-s.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go keepAlive(ctx, ws, s.manager.pingInterval)
}

// keepAlive pings an attached connection while it is quiet. A session can go
// minutes without a frame in either direction (it is signed for an idle
// timeout of up to an hour), and a load balancer between the edge and Kaana
// drops a connection that carries nothing for its own idle timeout. A ping
// that goes unanswered is left to the reader, which sees the connection end.
func keepAlive(ctx context.Context, ws *websocket.Conn, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingContext, cancel := context.WithTimeout(ctx, every)
			err := ws.Ping(pingContext)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// detach lets a connection go. The session keeps running, buffering what it
// emits, for the resume window.
func (s *session) detach(conn *connection, code websocket.StatusCode) {
	if conn == nil || s.attached != conn {
		return
	}
	s.attached = nil
	// The close handshake goes first: cancelling a connection's read context
	// drops it without a close frame, which the peer reads as an abnormal end.
	go func() {
		_ = conn.ws.Close(code, "")
		conn.cancel()
	}()
	if !s.closed {
		s.resumeTimer.Reset(s.manager.resumeWindow)
	}
}

// emit numbers one event, buffers it for replay and sends it to the attached
// connection, if any.
func (s *session) emit(ctx context.Context, event contract.RealtimeServerEvent, commandID *contract.RealtimeCommandID) {
	event.Stamp(contract.RealtimeEventHeader{SchemaVersion: contract.RealtimeSchemaVersion, RequestID: s.requestID, Sequence: s.sequence, CommandID: commandID})
	encoded, err := json.Marshal(event)
	if err != nil {
		// Every event is a contract type built here or by an adapter; one that
		// does not encode is a programming error, and the session cannot
		// continue with a gap in its sequence.
		s.manager.logger.Error("a realtime event did not encode", "requestId", s.requestID, "eventType", event.EventType())
		return
	}
	s.replay.push(s.sequence, encoded)
	s.sequence++
	s.write(ctx, encoded)
}

// write sends one frame to the attached connection. A write that fails means
// the connection is gone; the session detaches it rather than ending.
func (s *session) write(ctx context.Context, encoded []byte) {
	if s.attached == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.manager.writeTimeout)
	defer cancel()
	if err := s.attached.ws.Write(ctx, websocket.MessageText, encoded); err != nil {
		s.detach(s.attached, websocket.StatusGoingAway)
	}
}

func commandIDOf(frame []byte) *contract.RealtimeCommandID {
	var probe struct {
		CommandID string `json:"commandId"`
	}
	if json.Unmarshal(frame, &probe) != nil || probe.CommandID == "" || len(probe.CommandID) > 128 {
		return nil
	}
	id := contract.RealtimeCommandID(probe.CommandID)
	return &id
}

// receive handles one frame from a connection.
func (s *session) receive(ctx context.Context, frame inbound) {
	if frame.conn != s.attached {
		// A frame from a connection a resume has already replaced.
		return
	}
	if frame.err != nil {
		s.detach(frame.conn, websocket.StatusGoingAway)
		return
	}
	if frame.kind != websocket.MessageText {
		s.finish(ctx, contract.RealtimeClosedByClient,
			contract.NewError(s.requestID, contract.CodeInvalidRequest, "a realtime frame is one JSON text message; binary frames are refused"),
			nil, websocket.StatusUnsupportedData)
		return
	}
	command, err := contract.DecodeRealtimeCommand(frame.data, s.requestID)
	if err != nil {
		s.emit(ctx, &contract.RealtimeErrorEvent{Fatal: false, Error: *contract.NewError(s.requestID, contract.CodeInvalidRequest, err.Error())}, commandIDOf(frame.data))
		return
	}
	s.idle.Reset(time.Duration(s.request.Limits.IdleTimeoutMs) * time.Millisecond)
	_, commandID := command.Identity()
	if command.CommandType() == contract.RealtimeCommandSessionResumeType {
		s.emit(ctx, &contract.RealtimeErrorEvent{Fatal: false, Error: *contract.NewError(s.requestID, contract.CodeInvalidRequest,
			"session.resume is only ever the first frame of a new connection")}, &commandID)
		return
	}
	if _, duplicate := s.seen[commandID]; duplicate {
		s.emit(ctx, &contract.RealtimeCommandAcceptedEvent{Duplicate: true}, &commandID)
		return
	}

	// The signed limits are checked BEFORE the command is acknowledged: an
	// acknowledgement means the command was applied, and one that would pass a
	// ceiling is not applied at all.
	inputAudio := commandInputAudio(command)
	limits := s.request.Limits
	switch {
	case s.inputBytes+inputAudio > limits.MaxInputAudioBytes:
		s.finish(ctx, contract.RealtimeLimitExceeded, contract.NewError(s.requestID, contract.CodeRequestTooLarge,
			"the session's signed input audio ceiling would be exceeded"), &commandID, websocket.StatusNormalClosure)
		return
	case command.CommandType() == contract.RealtimeCommandResponseCreateType && s.responses >= limits.MaxResponses:
		s.finish(ctx, contract.RealtimeLimitExceeded, contract.NewError(s.requestID, contract.CodeOutputLimitExceeded,
			"the session's signed response ceiling has been reached"), &commandID, websocket.StatusNormalClosure)
		return
	}

	s.seen[commandID] = struct{}{}
	s.emit(ctx, &contract.RealtimeCommandAcceptedEvent{Duplicate: false}, &commandID)
	if command.CommandType() == contract.RealtimeCommandSessionCloseType {
		s.finish(ctx, contract.RealtimeClosedByClient, nil, nil, websocket.StatusNormalClosure)
		return
	}
	if err := s.opening.Upstream.Send(ctx, command); err != nil {
		var unsupported provider.ErrUnsupported
		if errors.As(err, &unsupported) {
			refusal := contract.NewError(s.requestID, unsupported.Code, unsupported.Detail)
			if unsupported.Param != "" {
				refusal = refusal.WithParam(unsupported.Param)
			}
			s.emit(ctx, &contract.RealtimeErrorEvent{Fatal: false, Error: *refusal}, &commandID)
			return
		}
		// The write to the provider failed, so whether it applied is
		// unknowable. It is never resent: the session ends here instead.
		s.finish(ctx, contract.RealtimeUpstreamError, upstreamError(s.requestID, s.opening.Route.Provider, err), &commandID, websocket.StatusNormalClosure)
		return
	}
	s.inputBytes += inputAudio
}

// commandInputAudio is the decoded audio a command carries into the session.
func commandInputAudio(command contract.RealtimeCommand) int {
	switch c := command.(type) {
	case *contract.RealtimeInputAudioAppendCommand:
		return contract.DecodedRealtimeAudioBytes(c.Data)
	case *contract.RealtimeItemCreateCommand:
		total := 0
		for _, part := range c.Item.Content {
			if part.Type == contract.RealtimeInputAudioPart && part.Data != nil {
				total += contract.DecodedRealtimeAudioBytes(*part.Data)
			}
		}
		return total
	}
	return 0
}

// upstream handles one event, or the end, of the provider session.
func (s *session) upstream(ctx context.Context, result upstreamResult) {
	if result.err != nil {
		switch {
		case errors.Is(result.err, provider.ErrRealtimeUpstreamClosed):
			s.finish(ctx, contract.RealtimeUpstreamClosed, nil, nil, websocket.StatusNormalClosure)
		case ctx.Err() != nil:
			s.finish(ctx, contract.RealtimeServerShutdown, nil, nil, websocket.StatusNormalClosure)
		default:
			s.finish(ctx, contract.RealtimeUpstreamError, upstreamError(s.requestID, s.opening.Route.Provider, result.err), nil, websocket.StatusNormalClosure)
		}
		return
	}
	for _, quantity := range result.event.Units {
		s.units[quantity.Unit] += quantity.Quantity
	}
	event := result.event.Event
	if event == nil {
		return
	}
	limits := s.request.Limits
	switch e := event.(type) {
	case *contract.RealtimeResponseCreatedEvent:
		if s.responses >= limits.MaxResponses {
			s.finish(ctx, contract.RealtimeLimitExceeded, contract.NewError(s.requestID, contract.CodeOutputLimitExceeded,
				"the session's signed response ceiling has been reached"), nil, websocket.StatusNormalClosure)
			return
		}
		s.responses++
	case *contract.RealtimeOutputAudioDeltaEvent:
		decoded := contract.DecodedRealtimeAudioBytes(e.Data)
		if s.outputBytes+decoded > limits.MaxOutputAudioBytes {
			s.finish(ctx, contract.RealtimeLimitExceeded, contract.NewError(s.requestID, contract.CodeOutputLimitExceeded,
				"the session's signed output audio ceiling would be exceeded"), nil, websocket.StatusNormalClosure)
			return
		}
		s.outputBytes += decoded
		s.markOutput()
	case *contract.RealtimeTextDeltaEvent, *contract.RealtimeTranscriptDeltaEvent, *contract.RealtimeToolCallEvent:
		s.markOutput()
	case *contract.RealtimeResponseDoneEvent:
		e.DeploymentID = s.opening.Route.DeploymentID
	}
	s.emit(ctx, event, result.event.CommandID)
}

func (s *session) markOutput() {
	if s.firstOutputAt.IsZero() {
		s.firstOutputAt = s.manager.now()
	}
}

// resume attaches a verified reconnection: every buffered event after the
// sequence the client names, in order, then session.resumed.
func (s *session) resume(ctx context.Context, request attachment) error {
	if s.closed {
		return errors.New("the session has ended")
	}
	missed, buffered := s.replay.after(request.command.AfterSequence, s.sequence)
	if !buffered {
		return fmt.Errorf("afterSequence %d is not a sequence this session can replay from", request.command.AfterSequence)
	}
	if s.attached != nil {
		// One connection per session: the resume replaces the old one.
		s.detach(s.attached, websocket.StatusGoingAway)
	}
	s.connect(ctx, request.ws)
	for _, encoded := range missed {
		s.write(ctx, encoded)
	}
	commandID := request.command.CommandID
	s.emit(ctx, &contract.RealtimeSessionResumedEvent{AfterSequence: request.command.AfterSequence}, &commandID)
	return nil
}

func upstreamError(requestID contract.RequestID, slug contract.ProviderSlug, err error) *contract.Error {
	var upstream provider.ErrUpstream
	if errors.As(err, &upstream) {
		return upstream.ContractError(requestID)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contract.NewError(requestID, contract.CodeProviderTimeout, fmt.Sprintf("%s did not respond in time", slug)).WithUpstream(contract.UpstreamTimeout, nil)
	}
	return contract.NewError(requestID, contract.CodeProviderError, fmt.Sprintf("the %s session failed", slug)).
		WithUpstream(contract.UpstreamUnknown, &contract.ProviderErrorPassthrough{Provider: slug})
}

// totals is the session's units, each once, in a stable order.
func (s *session) totals() []contract.UsageQuantity {
	units := make([]contract.UsageQuantity, 0, len(s.units))
	for unit, quantity := range s.units {
		units = append(units, contract.UsageQuantity{Unit: unit, Quantity: quantity})
	}
	sort.Slice(units, func(a, b int) bool { return units[a].Unit < units[b].Unit })
	return units
}

// finish ends the session exactly once: the fatal error if there is one,
// session.closed with the totals, settlement, the usage report to the
// connection attached at that moment, and the close.
func (s *session) finish(ctx context.Context, reason contract.RealtimeSessionCloseReason, failure *contract.Error, commandID *contract.RealtimeCommandID, code websocket.StatusCode) {
	if s.closed {
		return
	}
	s.closed = true
	s.idle.Stop()
	s.expiry.Stop()
	s.resumeTimer.Stop()
	if failure != nil {
		s.emit(ctx, &contract.RealtimeErrorEvent{Fatal: true, Error: *failure}, commandID)
	}
	closedAt := s.manager.now()
	opened := s.opening != nil && s.opening.Opened()
	source := contract.UsageProviderReported
	if opened {
		if meter, metered := s.opening.Upstream.(provider.RealtimeMeter); metered {
			// A provider billed by what Kaana measured: its measurement is
			// read once, here, and is the session's usage.
			for _, quantity := range meter.Measured() {
				s.units[quantity.Unit] += quantity.Quantity
			}
			source = contract.UsageOxyMeasured
		}
	}
	units := s.totals()
	closed := &contract.RealtimeSessionClosedEvent{Reason: reason, Units: units, UsageSource: source, ClosedAt: contract.NewTimestamp(closedAt)}
	if opened {
		deploymentID := s.opening.Route.DeploymentID
		closed.DeploymentID = &deploymentID
	} else {
		closed.Units = []contract.UsageQuantity{}
	}
	s.emit(ctx, closed, nil)
	if opened {
		_ = s.opening.Upstream.Close()
	}

	end := kaana.SessionEnd{ClosedAt: closedAt, Units: units, UsageSource: source, FirstOutputAt: s.firstOutputAt,
		Outcome: providercost.AttemptSucceeded, ReportOutcome: reportOutcome(reason, len(units) > 0, opened)}
	switch {
	case reason == contract.RealtimeUpstreamError:
		end.Outcome, end.FailureCode = providercost.AttemptFailed, contract.CodeProviderError
		if failure != nil {
			end.FailureCode = failure.Code
		}
	case reason == contract.RealtimeResumeExpired || reason == contract.RealtimeClosedByClient && failure != nil:
		end.Outcome = providercost.AttemptCancelled
	}
	settlement := s.manager.opener.SettleSession(ctx, s.opening, end)
	s.log(reason, failure, settlement)

	if s.attached != nil {
		if settlement.Report != nil {
			if encoded, err := json.Marshal(settlement.Report); err == nil {
				s.write(ctx, encoded)
			}
		}
		if s.attached != nil {
			conn := s.attached
			s.attached = nil
			_ = conn.ws.Close(code, "")
			conn.cancel()
		}
	}
}

// reportOutcome is how a session ends for settlement. A session that
// consumed nothing cannot be a completed request (a completed report carries
// at least one unit), so an empty one that ended normally is cancelled.
func reportOutcome(reason contract.RealtimeSessionCloseReason, measured, opened bool) contract.RequestOutcome {
	switch {
	case !opened:
		return contract.OutcomeFailed
	case reason == contract.RealtimeUpstreamError:
		if measured {
			return contract.OutcomePartial
		}
		return contract.OutcomeFailed
	case reason == contract.RealtimeResumeExpired || !measured:
		return contract.OutcomeCancelled
	}
	return contract.OutcomeCompleted
}

// log names ids, a route, an outcome, units and a duration — never a command,
// a transcript or audio. A session the upstream ended in failure also names
// that failure's code and Kaana's own message for it (for an event the adapter
// could not read, the event type and field: openairealtime's invalidEvent) —
// never the provider's passthrough text.
func (s *session) log(reason contract.RealtimeSessionCloseReason, failure *contract.Error, settlement kaana.SessionSettlement) {
	attributes := []any{"requestId", s.requestID, "reason", reason, "events", s.sequence,
		"durationMs", s.manager.now().Sub(s.opening.StartedAt()).Milliseconds(),
		"provider", s.opening.Route.Provider, "deploymentId", s.opening.Route.DeploymentID}
	if reason == contract.RealtimeUpstreamError && failure != nil {
		attributes = append(attributes, "errorCode", failure.Code, "error", failure.Message)
	}
	if settlement.Report != nil {
		attributes = append(attributes, "outcome", settlement.Report.Outcome, "units", settlement.Report.Units)
	}
	if len(settlement.UpstreamCost.Attempts) > 0 {
		attributes = append(attributes, "upstreamCost", settlement.UpstreamCost)
	}
	if settlement.CostRecordError != nil {
		s.manager.logger.Error("upstream provider cost was not persisted", "requestId", s.requestID, "errorType", "provider_cost_persistence")
	}
	if settlement.ReportError != nil {
		s.manager.logger.Error("the realtime usage report is not settleable", "requestId", s.requestID, "error", settlement.ReportError)
	}
	s.manager.logger.Info("realtime session closed", attributes...)
}
