package xaistt

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// writeTimeout bounds one frame written to xAI.
const writeTimeout = 10 * time.Second

// finalize is xAI's control message that ends the current utterance now.
var finalize = []byte(`{"type":"finalize"}`)

// session is one open speech-to-text session.
//
// xAI numbers nothing, so each utterance is an item Kaana names, `item_<n>`,
// opened by its first transcript and ended by its `speech_final`. Within it:
//
//   - an interim (`is_final: false`) is the text of the stretch not yet
//     locked; relayed, when the session asked for interim text, as
//     transcript.interim carrying the item's whole provisional text: what is
//     locked, then that stretch;
//   - a chunk final (`is_final: true, speech_final: false`) locks a stretch,
//     relayed as a transcript.delta that only appends;
//   - the utterance final (`speech_final: true`) is the complete stitched
//     utterance: what it adds to the locked text is one last delta, then
//     input_audio.committed and the item's one transcript.done, which carries
//     xAI's final text whatever the deltas said.
type session struct {
	conn      *websocket.Conn
	requestID contract.RequestID
	key       provider.Key
	interim   bool
	now       func() time.Time

	mu       sync.Mutex
	queue    []provider.RealtimeUpstreamEvent
	items    int
	current  contract.RealtimeItemID
	previous *contract.RealtimeItemID
	locked   string
	failure  error
	// readyAt is when xAI said the session was ready, endedAt when its
	// connection ended: the span the session is metered by.
	readyAt time.Time
	endedAt time.Time

	closeOnce sync.Once
}

func newSession(conn *websocket.Conn, request provider.RealtimeOpenRequest, key provider.Key, readyAt time.Time, now func() time.Time) *session {
	return &session{
		conn: conn, requestID: request.RequestID, key: key, now: now, readyAt: readyAt,
		interim: request.Config.InputAudioTranscription.WantsInterim(),
	}
}

// Send applies one command upstream, once: input audio as one binary frame,
// a commit as `finalize`. Every other command is refused before anything is
// written: a transcription session has no item, response or setting to
// change once open.
func (s *session) Send(ctx context.Context, command contract.RealtimeCommand) error {
	var (
		kind websocket.MessageType
		data []byte
	)
	switch c := command.(type) {
	case *contract.RealtimeInputAudioAppendCommand:
		audio, err := base64.StdEncoding.DecodeString(c.Data)
		if err != nil {
			return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "data", Detail: "the audio frame is not base64"}
		}
		kind, data = websocket.MessageBinary, audio
	case *contract.RealtimeInputAudioCommitCommand:
		kind, data = websocket.MessageText, finalize
	default:
		return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "type",
			Detail: fmt.Sprintf("a transcription session takes input_audio.append and input_audio.commit; %s is not served", command.CommandType())}
	}
	writeContext, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := s.conn.Write(writeContext, kind, data); err != nil {
		return transport(writeContext, err)
	}
	return nil
}

// Next returns the next normalized event.
func (s *session) Next(ctx context.Context) (provider.RealtimeUpstreamEvent, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			next := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return next, nil
		}
		s.mu.Unlock()

		kind, data, err := s.conn.Read(ctx)
		if err != nil {
			return provider.RealtimeUpstreamEvent{}, s.ended(ctx, err)
		}
		if kind != websocket.MessageText {
			return provider.RealtimeUpstreamEvent{}, invalidEvent("", "a binary frame")
		}
		event, err := decode(data)
		if err != nil {
			return provider.RealtimeUpstreamEvent{}, err
		}
		if event.Type == "error" {
			failure := classifyEvent(event.Message, s.key)
			s.mu.Lock()
			s.failure = failure
			s.mu.Unlock()
			return provider.RealtimeUpstreamEvent{}, failure
		}
		s.mu.Lock()
		s.translate(event)
		s.mu.Unlock()
	}
}

// translate queues the contract events one xAI event becomes; it is called
// with mu held.
func (s *session) translate(event serverEvent) {
	switch event.Type {
	case "transcript.partial":
		switch {
		case !event.IsFinal:
			if s.interim && event.Text != "" {
				item := s.open()
				s.push(&contract.RealtimeTranscriptInterimEvent{Source: contract.RealtimeTranscriptInput, ItemID: item, Text: joinText(s.locked, event.Text)})
			}
		case !event.SpeechFinal:
			if event.Text != "" {
				s.open()
				s.lock(event.Text)
			}
		default:
			if event.Text == "" && s.current == "" {
				return
			}
			s.finish(event.Text)
		}
	case "transcript.done":
		// Only after audio.done, which this adapter never sends: an
		// utterance still open ends on what is locked.
		if s.current != "" {
			s.finish(s.locked)
		}
	}
}

// open is the current item, opening one when there is none.
func (s *session) open() contract.RealtimeItemID {
	if s.current == "" {
		s.items++
		s.current = contract.RealtimeItemID("item_" + strconv.Itoa(s.items))
	}
	return s.current
}

// lock appends a stretch to the locked text, relayed as what it adds.
func (s *session) lock(text string) {
	joined := joinText(s.locked, text)
	if added := joined[len(s.locked):]; added != "" {
		s.push(&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: s.current, Text: added})
	}
	s.locked = joined
}

// finish ends the current item on its final text.
func (s *session) finish(final string) {
	item := s.open()
	if rest, extends := strings.CutPrefix(final, s.locked); extends && rest != "" {
		s.push(&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: item, Text: rest})
	}
	s.push(&contract.RealtimeInputAudioCommittedEvent{ItemID: item, PreviousItemID: s.previous})
	s.push(&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptInput, ItemID: item, Transcript: final})
	ended := item
	s.previous, s.current, s.locked = &ended, "", ""
}

func (s *session) push(event contract.RealtimeServerEvent) {
	s.queue = append(s.queue, provider.RealtimeUpstreamEvent{Event: event})
}

// joinText appends a stretch of transcript to what came before it, with one
// space between words when neither side brings its own.
func joinText(before, after string) string {
	switch {
	case before == "":
		return after
	case after == "":
		return before
	case strings.HasSuffix(before, " ") || strings.HasPrefix(after, " "):
		return before + after
	}
	return before + " " + after
}

// ended says why the connection to xAI ended: the session's own failure if
// xAI reported one, a clean end on a normal close, and a provider failure on
// anything else.
func (s *session) ended(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.mu.Lock()
	if s.endedAt.IsZero() {
		s.endedAt = s.now()
	}
	failure := s.failure
	s.mu.Unlock()
	if failure != nil {
		return failure
	}
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		return provider.ErrRealtimeUpstreamClosed
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError,
		Detail: "xAI's speech-to-text connection ended abnormally", Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

// Close ends the upstream session.
func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.endedAt.IsZero() {
			s.endedAt = s.now()
		}
		s.mu.Unlock()
		err = s.conn.Close(websocket.StatusNormalClosure, "")
	})
	return err
}

// Measured implements provider.RealtimeMeter: the session's wall clock, from
// xAI's transcript.created to the end of the connection (or now, while it is
// open), rounded up to the millisecond once. xAI prices streaming per hour
// without saying whether audio or connection time is counted; the wall clock
// bounds either (Kaana ADR 0006 §4).
func (s *session) Measured() []contract.UsageQuantity {
	s.mu.Lock()
	end := s.endedAt
	s.mu.Unlock()
	if end.IsZero() {
		end = s.now()
	}
	elapsed := end.Sub(s.readyAt)
	if elapsed < 0 {
		elapsed = 0
	}
	milliseconds := int((elapsed + time.Millisecond - 1) / time.Millisecond)
	return []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: milliseconds}}
}
