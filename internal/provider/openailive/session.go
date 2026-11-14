package openailive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// outputTurnGap is how long the voice is silent before what it said is one
// finished utterance, relayed as its transcript.done (Kaana ADR 0012 §2: "at a
// gap of 1.5 s in output deltas"). GPT-Live has no event that ends what it
// says: no done event for its audio and none for its transcript.
const outputTurnGap = 1500 * time.Millisecond

// closeTimeout bounds the wait for OpenAI's session.closed after Kaana asks for
// it at settlement. OpenAI's own example waits 15 s; Kaana settles inside its
// 30 s shutdown drain, so it waits 10.
const closeTimeout = 10 * time.Second

// writeTimeout bounds one client event written to OpenAI.
const writeTimeout = 10 * time.Second

// closeEventID names the session.close Kaana sends at settlement.
const closeEventID = "kaana-session-close"

// acknowledgementEventPrefix names the empty thinking append that suppresses
// a delegation's automatic acknowledgement: no client command caused it.
const acknowledgementEventPrefix = "kaana-acknowledgement-"

// session is one open GPT-Live session.
//
// One goroutine (read) reads OpenAI's events and translates them, in order,
// into a queue Next serves; Send writes from the session's goroutine. The
// reader, not Next, reads the connection because the transcript of the voice
// has no end event: a timer ends an utterance after outputTurnGap of silence,
// and only a reader that is always reading sees session.closed while the
// session is settling.
type session struct {
	// ctx is the session's life: it outlives the open that created it and
	// ends at Close.
	ctx         context.Context
	cancel      context.CancelFunc
	conn        *websocket.Conn
	requestID   contract.RequestID
	key         provider.Key
	format      contract.RealtimeAudioFormat
	suppressAck bool
	now         func() time.Time
	gap         time.Duration
	waitClosed  time.Duration
	handshakeAt time.Time

	// wake is signalled whenever the queue grows or the reader stops.
	wake chan struct{}
	// stopped is closed when the reader has stopped.
	stopped chan struct{}

	mu    sync.Mutex
	queue []provider.RealtimeUpstreamEvent
	// end is why Next ends once the queue is drained; set once, by the reader.
	end error
	// endedAt is when the reader stopped: the end of OpenAI's side.
	endedAt time.Time
	// failure is the last failure OpenAI reported about the session itself,
	// rather than about one client event: why the connection then ended.
	failure error
	// finalMs is session.closed's `usage.seconds` in milliseconds, rounded up
	// once: OpenAI's own final report of the session it bills.
	finalMs *int
	// delegations are the delegation ids OpenAI has opened.
	delegations map[contract.RealtimeDelegationID]bool
	// commands is the command each later chunk of a multi-append command was
	// written for; every other event is written with its command's id.
	commands map[string]contract.RealtimeCommandID
	input    turn
	output   turn
	// outputTimer ends the open utterance after the gap; generation tells a
	// stale timer from the current one.
	outputTimer *time.Timer
	generation  int

	closeOnce sync.Once
}

// turn is one item of transcript Kaana numbers: the person's words since the
// previous delegation, or one utterance of the voice.
type turn struct {
	count int
	open  bool
	text  string
	// lastEndMs is where the utterance's latest transcript fragment ended,
	// on OpenAI's session clock.
	lastEndMs *int64
}

func (t *turn) itemID(prefix string) contract.RealtimeItemID {
	return contract.RealtimeItemID(fmt.Sprintf("%s_%d", prefix, t.count))
}

func newSession(ctx context.Context, a *Adapter, conn *websocket.Conn, request provider.RealtimeOpenRequest, key provider.Key, handshakeAt time.Time) *session {
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		ctx: life, cancel: cancel, conn: conn, requestID: request.RequestID, key: key, format: request.Config.InputAudioFormat,
		suppressAck: request.Config.Delegation.Acknowledgement == contract.RealtimeAcknowledgementNone,
		now:         a.now, gap: a.outputGap, waitClosed: a.closeTimeout, handshakeAt: handshakeAt,
		wake: make(chan struct{}, 1), stopped: make(chan struct{}),
		delegations: make(map[contract.RealtimeDelegationID]bool), commands: make(map[string]contract.RealtimeCommandID),
	}
	go s.read()
	return s
}

var _ provider.RealtimeFinalUsage = (*session)(nil)

/* ----------------------------- commands ---------------------------------- */

// Send applies one command upstream, once. A command GPT-Live cannot express
// is refused before anything is written.
func (s *session) Send(ctx context.Context, command contract.RealtimeCommand) error {
	_, commandID := command.Identity()
	switch c := command.(type) {
	case *contract.RealtimeInputAudioAppendCommand:
		decoded, err := base64.StdEncoding.DecodeString(c.Data)
		if err != nil {
			return refused("data", "the audio is not base64")
		}
		if s.format == contract.RealtimePCM16 && len(decoded)%2 != 0 {
			// "PCM chunks must contain complete 16-bit samples".
			return refused("data", "pcm16_24khz audio is whole 16-bit samples")
		}
		return s.write(ctx, clientEvent{Type: "session.input_audio.append", EventID: string(commandID), Audio: c.Data})
	case *contract.RealtimeDelegationReplyCommand:
		// Spoken (paraphrased), each verbatim span between Kaana's marks.
		return s.appends(ctx, "session.commentary.append", commandID, c.DelegationID, splitAppend(provider.WrapVerbatim(c.Text, c.Verbatim)))
	case *contract.RealtimeDelegationContextCommand:
		// Silent context; only Kaana's own marks ever reach the voice.
		return s.appends(ctx, "session.thinking.append", commandID, c.DelegationID, splitAppend(provider.StripVerbatimMarks(c.Text)))
	case *contract.RealtimeDelegationCancelCommand:
		return s.appends(ctx, "session.instructions.append", commandID, c.DelegationID, []string{cancelInstruction})
	}
	return refused("type", "a live session's voice takes no "+string(command.CommandType()))
}

// appends writes one append event per part, in order: the first under the
// command's own id, the rest under ids that name it.
func (s *session) appends(ctx context.Context, eventType string, commandID contract.RealtimeCommandID, delegationID contract.RealtimeDelegationID, parts []string) error {
	id := string(delegationID)
	for index, part := range parts {
		eventID := string(commandID)
		if index > 0 {
			eventID = fmt.Sprintf("%s#%d", commandID, index)
			s.mu.Lock()
			s.commands[eventID] = commandID
			s.mu.Unlock()
		}
		if err := s.write(ctx, clientEvent{Type: eventType, EventID: eventID, DelegationID: &id, Content: &part}); err != nil {
			return err
		}
	}
	return nil
}

// write sends one client event. A provider that stops reading must not stall
// the session writing to it: a write that cannot complete in time ends the
// connection, and the session ends with it. A failed write is never resent.
func (s *session) write(ctx context.Context, event clientEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return refused("type", "the command could not be encoded for OpenAI")
	}
	writeContext, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := s.conn.Write(writeContext, websocket.MessageText, data); err != nil {
		return transport(writeContext, err)
	}
	return nil
}

/* ------------------------------ events ----------------------------------- */

// Next returns the next normalized event, then why the session ended.
func (s *session) Next(ctx context.Context) (provider.RealtimeUpstreamEvent, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			next := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return next, nil
		}
		end := s.end
		s.mu.Unlock()
		if end != nil {
			return provider.RealtimeUpstreamEvent{}, end
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return provider.RealtimeUpstreamEvent{}, ctx.Err()
		}
	}
}

func (s *session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// read is the reader: every event OpenAI sends, translated in order, until
// the session ends.
func (s *session) read() {
	for {
		kind, data, err := s.conn.Read(s.ctx)
		if err != nil {
			s.stop(s.connectionEnded(err))
			return
		}
		if kind != websocket.MessageText {
			s.stop(invalidEvent("", "a binary frame"))
			_ = s.conn.CloseNow()
			return
		}
		event, err := decode(data)
		var outcome translated
		if err == nil {
			outcome, err = s.translate(event)
		}
		if err == nil && outcome.suppress != nil {
			err = s.suppressAcknowledgement(*outcome.suppress)
		}
		if err == nil && outcome.end != nil {
			s.stop(outcome.end)
			// session.closed is OpenAI's last event; reading on lets its close
			// handshake complete, and nothing more is translated.
			for {
				if _, _, err := s.conn.Read(s.ctx); err != nil {
					return
				}
			}
		}
		if err != nil {
			s.stop(err)
			_ = s.conn.CloseNow()
			return
		}
	}
}

// connectionEnded says why the connection ended without session.closed: the
// session's own failure if OpenAI reported one, a clean end on a normal
// close, and OpenAI's failure on anything else.
func (s *session) connectionEnded(err error) error {
	s.mu.Lock()
	failure := s.failure
	s.mu.Unlock()
	switch {
	case failure != nil:
		return failure
	case websocket.CloseStatus(err) == websocket.StatusNormalClosure:
		return provider.ErrRealtimeUpstreamClosed
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError,
		Detail: "OpenAI's GPT-Live connection ended abnormally", Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

// stop ends the reader: what the person and the voice said so far is
// finished, then Next returns why the session ended.
func (s *session) stop(end error) {
	s.mu.Lock()
	if s.end == nil {
		s.queue = append(s.queue, s.finishInput()...)
		s.queue = append(s.queue, s.finishOutput()...)
		s.end, s.endedAt = end, s.now()
		close(s.stopped)
	}
	s.mu.Unlock()
	s.signal()
}

// push queues events for Next. Called with s.mu held.
func (s *session) push(events ...provider.RealtimeUpstreamEvent) {
	s.queue = append(s.queue, events...)
}

func event(value contract.RealtimeServerEvent) provider.RealtimeUpstreamEvent {
	return provider.RealtimeUpstreamEvent{Event: value}
}

// translated is what one server event asks of the reader beyond its events:
// a delegation whose automatic acknowledgement is to be suppressed, and, for
// session.closed, why the session ended.
type translated struct {
	suppress *contract.RealtimeDelegationID
	end      error
}

// translate maps one OpenAI server event to what the contract names.
func (s *session) translate(value serverEvent) (translated, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.signal()
	switch value.Type {
	case "error":
		if value.Error == nil {
			return translated{}, invalidEvent(value.Type, "error is absent")
		}
		s.push(s.errorEvent(*value.Error))

	case "session.input_transcript.delta":
		// The person's words, relayed whether or not transcription was asked
		// for: the live kind hands them over with every delegation.
		text := provider.StripVerbatimMarks(value.Delta)
		if text == "" {
			return translated{}, nil
		}
		if !s.input.open {
			s.input.count++
			s.input.open, s.input.text = true, ""
		}
		s.input.text += text
		s.push(event(&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: s.input.itemID("input"), Text: text}))

	case "session.output_transcript.delta":
		if value.StartMs == nil || value.EndMs == nil {
			return translated{}, invalidEvent(value.Type, "start_ms or end_ms is absent")
		}
		s.utterance(value.StartMs)
		s.output.lastEndMs = value.EndMs
		// The voice reads Kaana's marks as instructions; if its transcript
		// carries them, they are never relayed.
		if text := provider.StripVerbatimMarks(value.Delta); text != "" {
			s.output.text += text
			responseID := s.responseID()
			s.push(event(&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptOutput, ItemID: s.output.itemID("output"), ResponseID: &responseID, Text: text}))
		}

	case "session.output_audio.delta":
		audio, err := base64.StdEncoding.DecodeString(value.Delta)
		if err != nil || len(audio) == 0 {
			return translated{}, invalidEvent(value.Type, "delta is not base64 audio")
		}
		// Output audio carries no timing on the primary WebSocket: it belongs
		// to the utterance the voice is speaking, or begins one.
		s.utterance(nil)
		s.push(s.audioFrames(audio)...)

	case "session.delegation.created":
		return s.delegated(value)

	case "session.closed":
		return translated{end: s.closed(value)}, nil
	}
	// session.started after open, session.updated, the *.appended and
	// input_audio muted/unmuted acknowledgements, session.usage.updated (a
	// cumulative snapshot: the final figure is session.closed's), info, and
	// the Responses and Decisions events of delegation types Kaana does not
	// configure: nothing in the contract names them.
	return translated{}, nil
}

// errorEvent reports an in-band error without ending the session: most refuse
// one client event and the session stays open. A failure about the session
// itself is what the session reports it ended with when the connection ends.
func (s *session) errorEvent(value wireError) provider.RealtimeUpstreamEvent {
	failure := classifyEvent(value, s.key)
	var upstream provider.ErrUpstream
	if !errors.As(failure, &upstream) {
		upstream = provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "OpenAI's GPT-Live session failed"}
	}
	if upstream.Category != contract.UpstreamInvalidReq {
		s.failure = failure
	}
	result := event(&contract.RealtimeErrorEvent{Fatal: false, Error: *upstream.ContractError(s.requestID)})
	if value.ClientEventID != nil {
		if commandID, named := s.commandFor(*value.ClientEventID); named {
			result.CommandID = &commandID
		}
	}
	return result
}

// commandFor is the command a client event id was written for: its own id, or
// the id a later chunk was recorded under. Kaana's own events (the start, the
// close, an acknowledgement suppression) answer no command.
func (s *session) commandFor(eventID string) (contract.RealtimeCommandID, bool) {
	if commandID, chunk := s.commands[eventID]; chunk {
		return commandID, true
	}
	if eventID == "" || eventID == startEventID || eventID == closeEventID || len(eventID) > 128 ||
		len(eventID) >= len(acknowledgementEventPrefix) && eventID[:len(acknowledgementEventPrefix)] == acknowledgementEventPrefix {
		return "", false
	}
	return contract.RealtimeCommandID(eventID), true
}

// delegated hands one turn over: the person's transcript since the previous
// delegation becomes that item's transcript.done and the delegation's text.
// session.delegation.created carries no words (live-delegation guide).
//
// UNVERIFIED: that the input transcript of a turn always arrives before the
// delegation of it; a fragment that arrives after is the next turn's.
func (s *session) delegated(value serverEvent) (translated, error) {
	if value.Delegation == nil {
		return translated{}, invalidEvent(value.Type, "delegation is absent")
	}
	id := contract.RealtimeDelegationID(value.Delegation.ID)
	units := len(utf16.Encode([]rune(value.Delegation.ID)))
	switch {
	case value.Delegation.Target != "client":
		return translated{}, invalidEvent(value.Type, "delegation.target is not client")
	case units < 1 || units > 128:
		return translated{}, invalidEvent(value.Type, "delegation.id is not 1 to 128 characters")
	case s.delegations[id]:
		return translated{}, invalidEvent(value.Type, "delegation.id repeats an earlier delegation")
	}
	s.delegations[id] = true
	text := s.input.text
	s.push(s.finishInput()...)
	s.push(event(&contract.RealtimeDelegationRequestedEvent{DelegationID: id, Text: text}))
	if s.suppressAck {
		return translated{suppress: &id}, nil
	}
	return translated{}, nil
}

// suppressAcknowledgement answers a delegation of a session that asked for no
// acknowledgement with an empty thinking append, which cancels the pending
// one (live-delegation guide, "Automatic acknowledgments").
func (s *session) suppressAcknowledgement(id contract.RealtimeDelegationID) error {
	delegationID, empty := string(id), ""
	return s.write(s.ctx, clientEvent{Type: "session.thinking.append", EventID: acknowledgementEventPrefix + delegationID,
		DelegationID: &delegationID, Content: &empty})
}

// finishInput ends the person's open item: its one transcript.done, the whole
// text its deltas carried. Called with s.mu held.
func (s *session) finishInput() []provider.RealtimeUpstreamEvent {
	if !s.input.open {
		return nil
	}
	s.input.open = false
	text := s.input.text
	s.input.text = ""
	return []provider.RealtimeUpstreamEvent{event(&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptInput, ItemID: s.input.itemID("input"), Transcript: text})}
}

// utterance is the voice's utterance an output fragment belongs to: the open
// one, unless the fragment starts outputTurnGap or more after the open one's
// last fragment ended on OpenAI's session clock, in which case that one is
// finished and a new one begins. Every fragment re-arms the timer that
// finishes the utterance after the same gap of silence on Kaana's clock.
// Called with s.mu held.
func (s *session) utterance(startMs *int64) {
	if s.output.open && startMs != nil && s.output.lastEndMs != nil && *startMs-*s.output.lastEndMs >= s.gap.Milliseconds() {
		s.push(s.finishOutput()...)
	}
	if !s.output.open {
		s.output.count++
		s.output.open, s.output.text, s.output.lastEndMs = true, "", nil
	}
	s.generation++
	generation := s.generation
	if s.outputTimer != nil {
		s.outputTimer.Stop()
	}
	s.outputTimer = time.AfterFunc(s.gap, func() { s.silent(generation) })
}

// silent finishes the voice's utterance when no output followed it for the
// gap.
func (s *session) silent(generation int) {
	s.mu.Lock()
	if s.end == nil && s.output.open && s.generation == generation {
		s.push(s.finishOutput()...)
	}
	s.mu.Unlock()
	s.signal()
}

// finishOutput ends the voice's open utterance: its one transcript.done when
// it said anything. Its audio has no done event (GPT-Live sends none), so
// output_audio.done is never sent for a live session. Called with s.mu held.
func (s *session) finishOutput() []provider.RealtimeUpstreamEvent {
	if !s.output.open {
		return nil
	}
	s.output.open = false
	if s.outputTimer != nil {
		s.outputTimer.Stop()
	}
	if s.output.text == "" {
		return nil
	}
	responseID := s.responseID()
	text := s.output.text
	s.output.text = ""
	return []provider.RealtimeUpstreamEvent{event(&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptOutput, ItemID: s.output.itemID("output"), ResponseID: &responseID, Transcript: text})}
}

// responseID names the voice's open utterance as the response its audio and
// transcript belong to: GPT-Live has no responses, so Kaana numbers them.
func (s *session) responseID() contract.RealtimeResponseID {
	return contract.RealtimeResponseID(fmt.Sprintf("response_%d", s.output.count))
}

// audioFrames splits one output audio delta into contract-bounded frames
// (provider.MaxAudioChunkBytes decoded bytes each), in order: OpenAI
// documents no bound on a delta. Called with s.mu held.
func (s *session) audioFrames(audio []byte) []provider.RealtimeUpstreamEvent {
	frames := make([]provider.RealtimeUpstreamEvent, 0, len(audio)/provider.MaxAudioChunkBytes+1)
	for start := 0; start < len(audio); start += provider.MaxAudioChunkBytes {
		end := min(start+provider.MaxAudioChunkBytes, len(audio))
		frames = append(frames, event(&contract.RealtimeOutputAudioDeltaEvent{
			ResponseID: s.responseID(), ItemID: s.output.itemID("output"), Format: s.format,
			Data: base64.StdEncoding.EncodeToString(audio[start:end]),
		}))
	}
	return frames
}

// closed reads OpenAI's final event: the session's billed seconds and why it
// ended. Called with s.mu held.
func (s *session) closed(value serverEvent) error {
	if value.Usage == nil || value.Usage.Seconds == nil {
		return invalidEvent(value.Type, "usage.seconds is absent")
	}
	milliseconds, ok := ceilMilliseconds(*value.Usage.Seconds)
	if !ok {
		return invalidEvent(value.Type, "usage.seconds is not a non-negative number")
	}
	s.finalMs = &milliseconds
	if s.failure != nil {
		return s.failure
	}
	passthrough := &contract.ProviderErrorPassthrough{Provider: Slug}
	switch value.Reason {
	case "close_requested", "expired", "remote_hangup":
		// Kaana's own close, the session's duration limit, and the remote
		// primary connection ending gracefully: an end, not a failure.
		return provider.ErrRealtimeUpstreamClosed
	case "content":
		return provider.ErrUpstream{Code: contract.CodeUpstreamContentFiltered, Category: contract.UpstreamContentFilter,
			Detail: "OpenAI's safety filter ended the GPT-Live session", Passthrough: passthrough}
	case "connection_lost":
		return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError,
			Detail: "OpenAI lost the GPT-Live session's connection", Passthrough: passthrough}
	}
	return invalidEvent(value.Type, "reason is not one this adapter knows")
}

// ceilMilliseconds is a JSON number of seconds in whole milliseconds, rounded
// up once, read exactly: OpenAI bills per second and never rounds up to the
// minute, and a float would read 12.3 s as 12 300.000000000002 ms.
func ceilMilliseconds(seconds json.Number) (int, bool) {
	value, ok := new(big.Rat).SetString(string(seconds))
	if !ok || value.Sign() < 0 {
		return 0, false
	}
	value.Mul(value, big.NewRat(1000, 1))
	quotient, remainder := new(big.Int).QuoRem(value.Num(), value.Denom(), new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Int64() > 1<<53 {
		return 0, false
	}
	return int(quotient.Int64()), true
}

/* ---------------------------- settlement ---------------------------------- */

// FinalUsage implements provider.RealtimeFinalUsage. OpenAI bills a session by
// its seconds and reports them on session.closed, after which the connection
// ends; a session Kaana ends is first asked to close (session.close, then
// session.closed, as the voice-websockets guide says to wait for it). Without
// session.closed the usage is unconfirmed (OpenAI: "If the transport fails
// before session.closed, final usage is unconfirmed"), so the session settles
// Kaana's own wall clock, from the accepted handshake to the end of the
// connection, rounded up once and labelled oxy_measured: the clock bounds
// what OpenAI can bill.
func (s *session) FinalUsage(ctx context.Context) ([]contract.UsageQuantity, contract.UsageSource) {
	s.mu.Lock()
	running := s.end == nil
	s.mu.Unlock()
	if running {
		if err := s.write(ctx, clientEvent{Type: "session.close", EventID: closeEventID}); err == nil {
			timer := time.NewTimer(s.waitClosed)
			select {
			case <-s.stopped:
			case <-timer.C:
			}
			timer.Stop()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalMs != nil {
		return sessionMilliseconds(*s.finalMs), contract.UsageProviderReported
	}
	end := s.endedAt
	if end.IsZero() {
		end = s.now()
	}
	elapsed := max(end.Sub(s.handshakeAt), 0)
	return sessionMilliseconds(int((elapsed + time.Millisecond - 1) / time.Millisecond)), contract.UsageOxyMeasured
}

// sessionMilliseconds is the session's one unit; a session that lasted no
// time reports none rather than a zero.
func sessionMilliseconds(milliseconds int) []contract.UsageQuantity {
	if milliseconds <= 0 {
		return []contract.UsageQuantity{}
	}
	return []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: milliseconds}}
}

// Close ends the upstream session. The data plane never resumes it, so there
// is nothing to keep.
func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.outputTimer != nil {
			s.outputTimer.Stop()
		}
		s.mu.Unlock()
		err = s.conn.Close(websocket.StatusNormalClosure, "")
		s.cancel()
	})
	return err
}
