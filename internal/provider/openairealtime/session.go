package openairealtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// maxAudioFrameBytes is provider.MaxAudioChunkBytes, the one bound on raw
// audio in a contract audio event, which is also the decoded size of the
// largest realtime frame (MAX_REALTIME_AUDIO_FRAME_BASE64_LENGTH characters of
// padded base64). OpenAI documents no bound on an output audio delta, so a
// larger one is split into frames of at most this many bytes, in order. The
// frames are contract events rather than Emitter calls, which is why this is
// not provider.EmitAudio.
const maxAudioFrameBytes = provider.MaxAudioChunkBytes

// writeTimeout bounds one client event written to the provider.
const writeTimeout = 10 * time.Second

type pendingUpdate struct {
	commandID contract.RealtimeCommandID
	update    contract.RealtimeSessionConfigUpdate
}

// session is one open Realtime conversation, in one provider's dialect.
type session struct {
	dialect      *dialect
	conn         *websocket.Conn
	requestID    contract.RequestID
	key          provider.Key
	inputFormat  contract.RealtimeAudioFormat
	outputFormat *contract.RealtimeAudioFormat

	mu sync.Mutex
	// config is the effective configuration: the signed one, with every update
	// OpenAI has confirmed applied in the order it confirmed them.
	config contract.RealtimeSessionConfig
	// pending is the session.update commands sent and not yet confirmed.
	// OpenAI confirms with a session.updated that does not echo the event id,
	// in the order the updates were sent, or refuses one with an error that
	// does.
	pending []pendingUpdate
	// toolNames is each streamed call's function name, which OpenAI states on
	// the call's output item and never on its argument deltas.
	toolNames map[string]string
	namedTool map[string]bool
	streamed  map[string]bool
	queue     []provider.RealtimeUpstreamEvent
	// failure is the last failure the provider reported about the session
	// itself rather than about one event, which is why the connection then
	// ended.
	failure error
	// meter is what Kaana measures for a provider that bills by it rather
	// than by the tokens it reports (meter.go); nil for OpenAI.
	meter *meter
	// openResponse is the in-conversation response created and not yet done.
	// Both providers run at most one (Kaana's contract has no out-of-band
	// response), so a response.created while one is open means the provider
	// ended it: xAI cancels an answer the caller talks over and sends no
	// response.done for it (measured 2026-10-16), which left every client
	// counting a response that never ends.
	openResponse contract.RealtimeResponseID
	// expected is the output modalities each response.create the client sent
	// asked for (nil: the session's), in order, until the provider creates
	// that response; asked is what each created response was asked for. What
	// a response relays to the client is what it asked for, whatever the
	// provider was asked for to give it (relayModalities).
	expected []expectedResponse
	asked    map[contract.RealtimeResponseID][]contract.RealtimeOutputModality
	// inputs is each caller audio item's transcript so far (inputTranscript).
	inputs map[string]*inputItem
	// awaiting are the committed items without their final transcript yet,
	// in the order they were committed.
	awaiting []string
	// lastInput is the caller's latest item, and responseEnded a response
	// having ended (the provider's response.done) since it began: what a
	// transcriber that continues interrupted turns carries into the next
	// item (inputItem).
	lastInput     *inputItem
	responseEnded bool

	closeOnce sync.Once
}

type expectedResponse struct {
	modalities []contract.RealtimeOutputModality
}

// inputItem is one item of the caller's audio, transcribed.
type inputItem struct {
	// committed is the item's audio having been committed: the provider's
	// next completed transcript of it is the final one.
	committed bool
	// transcribed is the provider's latest transcript of the item, whole:
	// on a provider that continues interrupted turns, it starts with the
	// text of the items before it (carried).
	transcribed string
	// carried is the earlier item whose transcript the provider's transcripts
	// of this one start with; nil when they hold only this item's audio.
	carried *inputItem
	// text is the transcript relayed so far as deltas: this item's own words.
	text string
	// done is the item's one transcript.done having been relayed.
	done         bool
	contentIndex int
}

func newSession(d *dialect, conn *websocket.Conn, request provider.RealtimeOpenRequest, key provider.Key, inputFormat contract.RealtimeAudioFormat) *session {
	return &session{
		dialect: d, conn: conn, requestID: request.RequestID, key: key,
		inputFormat: inputFormat, outputFormat: d.wireOutputFormat(request.Config), config: request.Config,
		toolNames: make(map[string]string), namedTool: make(map[string]bool), streamed: make(map[string]bool),
		asked: make(map[contract.RealtimeResponseID][]contract.RealtimeOutputModality), inputs: make(map[string]*inputItem),
	}
}

// Send applies one command upstream, once.
func (s *session) Send(ctx context.Context, command contract.RealtimeCommand) error {
	event, err := s.wireCommand(command)
	if err != nil {
		return err
	}
	if update, ok := command.(*contract.RealtimeSessionUpdateCommand); ok {
		if err := s.admitUpdate(update.Config); err != nil {
			return err
		}
	}
	var billed measurement
	if s.meter != nil {
		if billed, err = s.meter.measure(command); err != nil {
			return err
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return refused("type", "the command could not be encoded for "+s.dialect.name)
	}
	if update, ok := command.(*contract.RealtimeSessionUpdateCommand); ok {
		s.mu.Lock()
		s.pending = append(s.pending, pendingUpdate{commandID: update.CommandID, update: update.Config})
		s.mu.Unlock()
	}
	// A provider that stops reading must not stall the session that is
	// writing to it: a write that cannot complete in time ends the
	// connection, and the session ends with it.
	writeContext, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := s.conn.Write(writeContext, websocket.MessageText, data); err != nil {
		return s.dialect.transport(writeContext, err)
	}
	// Recorded only once the provider has it: a command that never left is
	// not billed.
	if s.meter != nil {
		s.meter.record(billed)
	}
	return nil
}

// admitUpdate refuses, before anything is written, an update whose effect the
// provider does not document how to bill: switching between a clock-billed
// turn detection and one billed by audio (xAI documents each mode's billing,
// not a session that changes mode). An update the statement does not serve is
// refused by refuseIssues, before this.
func (s *session) admitUpdate(update contract.RealtimeSessionConfigUpdate) error {
	d := s.dialect
	if update.TurnDetection == nil || !d.billsClock() {
		return nil
	}
	s.mu.Lock()
	turnDetection := s.config.TurnDetection.Type
	for _, pending := range s.pending {
		if pending.update.TurnDetection != nil {
			turnDetection = pending.update.TurnDetection.Type
		}
	}
	s.mu.Unlock()
	if d.clockBilled(update.TurnDetection.Type) != d.clockBilled(turnDetection) {
		return refused("config.turnDetection", fmt.Sprintf("%s bills a session by its duration under one turn detection and by its audio under another, and does not document a session that changes between them; open a new session instead", d.name))
	}
	return nil
}

// projectedConfig is the configuration in effect for the next command: the
// confirmed one, with every update sent since applied in order, as the
// provider applies client events in order.
func (s *session) projectedConfig() contract.RealtimeSessionConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	config := s.config
	for _, pending := range s.pending {
		config = mergeUpdate(config, pending.update)
	}
	return config
}

// expectResponse records what a response.create the client sent asked for.
func (s *session) expectResponse(modalities []contract.RealtimeOutputModality) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expected = append(s.expected, expectedResponse{modalities: modalities})
}

// relayModalities is what a response asked for: its response.create's, or
// the session's for one the provider created itself (server_vad).
func (s *session) relayModalities(id contract.RealtimeResponseID) []contract.RealtimeOutputModality {
	s.mu.Lock()
	defer s.mu.Unlock()
	if asked, ok := s.asked[id]; ok && asked != nil {
		return asked
	}
	return s.config.OutputModalities
}

// emulated reports a response whose asked modalities the wire does not take
// (WireOutputModalities asked it for audio instead): its text is the
// transcript of that audio, and its audio is relayed only if it was asked for.
func (s *session) emulated(id contract.RealtimeResponseID) (asked []contract.RealtimeOutputModality, emulated bool) {
	asked = s.relayModalities(id)
	return asked, asked != nil && !s.dialect.statement().Parameters.ServesOutputModalities(asked)
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
			return provider.RealtimeUpstreamEvent{}, s.dialect.invalidEvent("", "a binary frame")
		}
		event, err := s.dialect.decode(data)
		if err != nil {
			return provider.RealtimeUpstreamEvent{}, err
		}
		if alias, named := s.dialect.aliases[event.Type]; named {
			event.Type = alias
		}
		events, err := s.translate(event)
		if err != nil {
			return provider.RealtimeUpstreamEvent{}, err
		}
		if len(events) == 0 {
			continue
		}
		s.mu.Lock()
		s.queue = append(s.queue, events[1:]...)
		s.mu.Unlock()
		return events[0], nil
	}
}

// ended says why the connection to the provider ended: the session's own
// failure if the provider reported one, a clean end on a normal close, and a
// provider failure on anything else.
func (s *session) ended(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.meter != nil {
		// The provider's side of the session is over: its clock stops here.
		s.meter.end()
	}
	s.mu.Lock()
	failure := s.failure
	s.mu.Unlock()
	if failure != nil {
		return failure
	}
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		return provider.ErrRealtimeUpstreamClosed
	}
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamServerError,
		Detail: s.dialect.name + "'s Realtime connection ended abnormally", Passthrough: &contract.ProviderErrorPassthrough{Provider: s.dialect.slug}}
}

// Close ends the upstream session. The data plane never resumes it, so there
// is nothing to keep.
func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.meter != nil {
			s.meter.end()
		}
		err = s.conn.Close(websocket.StatusNormalClosure, "")
	})
	return err
}

func one(event contract.RealtimeServerEvent) []provider.RealtimeUpstreamEvent {
	return []provider.RealtimeUpstreamEvent{{Event: event}}
}

// translate maps one provider server event to what the contract names. An event
// type the contract lacks is dropped here, never forwarded.
func (s *session) translate(event serverEvent) ([]provider.RealtimeUpstreamEvent, error) {
	switch event.Type {
	case "error":
		if event.Error == nil {
			return nil, s.dialect.invalidEvent(event.Type, "error is absent")
		}
		return s.errorEvent(*event.Error), nil

	case "session.updated":
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.mu.Unlock()
			return nil, nil
		}
		confirmed := s.pending[0]
		s.pending = s.pending[1:]
		s.config = mergeUpdate(s.config, confirmed.update)
		config := s.config
		s.mu.Unlock()
		commandID := confirmed.commandID
		return []provider.RealtimeUpstreamEvent{{Event: &contract.RealtimeSessionUpdatedEvent{Config: config}, CommandID: &commandID}}, nil

	case "conversation.item.added", "conversation.item.done":
		if event.Item == nil {
			return nil, s.dialect.invalidEvent(event.Type, "item is absent")
		}
		s.rememberToolName(*event.Item)
		value, expressible := s.contractItem(*event.Item)
		if !expressible {
			// A message with no content yet, an MCP item, an image part: the
			// contract has no faithful shape for it, so it is not forwarded.
			return nil, nil
		}
		if event.Type == "conversation.item.done" {
			return one(&contract.RealtimeItemDoneEvent{ItemID: *value.ItemID, Item: value}), nil
		}
		return one(&contract.RealtimeItemAddedEvent{ItemID: *value.ItemID, PreviousItemID: itemID(event.PreviousItemID), Item: value}), nil

	case "conversation.item.deleted":
		return one(&contract.RealtimeItemDeletedEvent{ItemID: contract.RealtimeItemID(event.ItemID)}), nil
	case "conversation.item.truncated":
		return one(&contract.RealtimeItemTruncatedEvent{ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex, AudioEndMs: event.AudioEndMs}), nil
	case "input_audio_buffer.speech_started":
		s.inputBegun(event.ItemID)
		return one(&contract.RealtimeSpeechStartedEvent{ItemID: contract.RealtimeItemID(event.ItemID), AudioStartMs: event.AudioStartMs}), nil
	case "input_audio_buffer.speech_stopped":
		return one(&contract.RealtimeSpeechStoppedEvent{ItemID: contract.RealtimeItemID(event.ItemID), AudioEndMs: event.AudioEndMs}), nil
	case "input_audio_buffer.committed":
		events := s.inputCommitted(event.ItemID)
		return append(events, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeInputAudioCommittedEvent{ItemID: contract.RealtimeItemID(event.ItemID), PreviousItemID: itemID(event.PreviousItemID)}}), nil
	case "input_audio_buffer.cleared":
		return one(&contract.RealtimeInputAudioClearedEvent{}), nil

	case "response.created":
		if event.Response == nil || event.Response.ID == "" {
			return nil, s.dialect.invalidEvent(event.Type, "response.id is absent")
		}
		if _, err := s.dialect.statusReason(event.Type, *event.Response); err != nil {
			return nil, err
		}
		created := contract.RealtimeResponseID(event.Response.ID)
		s.mu.Lock()
		ended := s.openResponse
		s.openResponse = created
		if len(s.expected) > 0 {
			s.asked[created] = s.expected[0].modalities
			s.expected = s.expected[1:]
		}
		s.mu.Unlock()
		events := one(&contract.RealtimeResponseCreatedEvent{ResponseID: created})
		if ended != "" && ended != created {
			// Every response.created gets its response.done, in order.
			events = append([]provider.RealtimeUpstreamEvent{{Event: s.endedUnannounced(ended)}}, events...)
		}
		return events, nil
	case "response.output_item.added", "response.output_item.done":
		if event.Item != nil {
			s.rememberToolName(*event.Item)
		}
		return nil, nil

	case "response.output_audio.delta":
		frames, err := s.audioFrames(event)
		if err != nil || s.relaysAudio(contract.RealtimeResponseID(event.ResponseID)) {
			return frames, err
		}
		return nil, nil
	case "response.output_audio.done":
		if !s.relaysAudio(contract.RealtimeResponseID(event.ResponseID)) {
			return nil, nil
		}
		return one(&contract.RealtimeOutputAudioDoneEvent{ResponseID: contract.RealtimeResponseID(event.ResponseID), ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex}), nil
	case "response.output_audio_transcript.delta":
		return s.outputTranscript(event, false), nil
	case "response.output_audio_transcript.done":
		return s.outputTranscript(event, true), nil
	case "response.output_text.delta":
		return one(&contract.RealtimeTextDeltaEvent{ResponseID: contract.RealtimeResponseID(event.ResponseID), ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex, Text: event.Delta}), nil

	case "response.function_call_arguments.delta":
		return one(s.toolCall(event, event.Delta, false)), nil
	case "response.function_call_arguments.done":
		return one(s.toolCall(event, event.Arguments, true)), nil

	case "response.done":
		return s.responseDone(event)

	case "conversation.item.input_audio_transcription.delta":
		if !s.transcribingInput() || event.Delta == "" {
			return nil, nil
		}
		return s.inputRunning(event.ItemID, event.ContentIndex, func(text string) string { return text + event.Delta }), nil
	case "conversation.item.input_audio_transcription.updated":
		// xAI's running transcript of the item, whole each time.
		if !s.transcribingInput() {
			return nil, nil
		}
		return s.inputRunning(event.ItemID, event.ContentIndex, func(string) string { return event.Transcript }), nil
	case "conversation.item.input_audio_transcription.completed":
		return s.inputTranscript(event)
	case "conversation.item.input_audio_transcription.failed":
		return s.inputTranscriptionFailed(event)
	}
	// session.created after open, conversation.created, rate_limits.updated,
	// content parts, output_text.done (the deltas already carried it),
	// input_audio_buffer.timeout_triggered, WebRTC-only output buffer events,
	// MCP events and OpenAI's diarized transcription segments: nothing in the
	// contract names them.
	return nil, nil
}

// transcribingInput is whether the caller has asked for its input to be
// transcribed: at open, or by a session.update the provider has confirmed
// (the effective configuration). Only then is an input transcript relayed; a
// transcription the provider bills is settled either way.
func (s *session) transcribingInput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.InputAudioTranscription != nil
}

// The caller's transcript: exactly one transcript.done per input item, the
// final one, after the deltas that led to it (Kaana ADR 0003). OpenAI commits
// an item and then sends one completed transcript. xAI sends a running
// transcript and a "completed" one at every pause, repeats them, reuses the
// item across pauses, and sends its final one only after the item is
// committed (measured 2026-10-16). Both are one rule: an item's final
// transcript is the first completed one after its commit; one before is a
// running transcript, and one after the final is a repeat. A running
// transcript is relayed as the deltas that extend what was relayed; one that
// rewrites it adds no delta (the contract's delta only appends), and the
// final transcript.done carries the whole text.
//
// Each item's transcript is the text of that item's audio only (Kaana ADR
// 0003). xAI transcribes an item the caller starts by talking over a response
// together with the items before it (continuesInterruptedTurns); what it
// carried is cut here (ownText), so the client never sees a phrase twice.

// inputItem is the item id names, begun now if this is the first event about
// it. Called with s.mu held.
func (s *session) inputItem(id string, contentIndex int) *inputItem {
	item, ok := s.inputs[id]
	if !ok {
		item = &inputItem{contentIndex: contentIndex}
		if s.dialect.transcription.continuesInterruptedTurns && !s.responseEnded {
			item.carried = s.lastInput
		}
		s.lastInput, s.responseEnded = item, false
		s.inputs[id] = item
	}
	return item
}

// inputBegun notes an item of the caller's speech when the provider reports it
// started, so that what a transcriber carries into it is decided when its
// audio began, not when its first transcript arrives.
func (s *session) inputBegun(id string) {
	if !s.transcribingInput() || id == "" {
		return
	}
	s.mu.Lock()
	s.inputItem(id, 0)
	s.mu.Unlock()
}

// inputRunning relays the extension of an item's running transcript; next
// computes the provider's whole transcript from its previous one.
func (s *session) inputRunning(id string, contentIndex int, next func(previous string) string) []provider.RealtimeUpstreamEvent {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	item := s.inputItem(id, contentIndex)
	if item.done {
		s.mu.Unlock()
		return nil
	}
	item.transcribed = next(item.transcribed)
	text := item.ownText()
	delta, extends := strings.CutPrefix(text, item.text)
	if !extends || delta == "" {
		s.mu.Unlock()
		return nil
	}
	item.text = text
	s.mu.Unlock()
	return one(&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: contract.RealtimeItemID(id), ContentIndex: item.contentIndex, Text: delta})
}

// ownText is the part of the provider's transcript of an item that is the
// item's own audio: what follows the words of the item it carried, matched
// word by word ignoring case and punctuation (which a transcriber revises as
// it hears more). A transcript that has not yet passed those words has none
// of its own so far; one that no longer starts with them is the item's whole,
// so that no word the caller said is lost.
func (item *inputItem) ownText() string {
	if item.carried == nil {
		return item.transcribed
	}
	rest := item.transcribed
	for _, word := range strings.Fields(item.carried.transcribed) {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			end = len(rest)
		}
		heard, carried := wordKey(rest[:end]), wordKey(word)
		switch {
		case heard == carried:
			rest = rest[end:]
		case end == len(rest) && strings.HasPrefix(carried, heard):
			// The transcript ends inside the carried words.
			return ""
		default:
			return item.transcribed
		}
	}
	return strings.TrimLeftFunc(rest, unicode.IsSpace)
}

// wordKey is a word as a transcriber's revisions leave it: lower case, letters
// and digits only.
func wordKey(word string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, word)
}

// inputCommitted marks an item committed: its next completed transcript is
// final. An earlier item that was committed and is still waiting for its
// final transcript is given the one it has, so no item stays without one.
func (s *session) inputCommitted(id string) []provider.RealtimeUpstreamEvent {
	if !s.transcribingInput() || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var finals []provider.RealtimeUpstreamEvent
	awaiting := s.awaiting[:0]
	for _, other := range s.awaiting {
		item := s.inputs[other]
		switch {
		case item.done:
		case other == id:
			awaiting = append(awaiting, other)
		case item.text != "":
			item.done = true
			finals = append(finals, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptInput, ItemID: contract.RealtimeItemID(other), ContentIndex: item.contentIndex, Transcript: item.text}})
		default:
			awaiting = append(awaiting, other)
		}
	}
	item := s.inputItem(id, 0)
	if !item.committed && !item.done {
		item.committed = true
		awaiting = append(awaiting, id)
	}
	s.awaiting = awaiting
	return finals
}

// inputTranscript is one completed transcript of the caller's speech. Its
// units are what the provider bills the transcription model for
// (transcriptionUnits) and are settled whatever is relayed: a charge the
// provider made is never dropped. xAI's event carries no content_index; the
// item it names is the caller's audio item, whose one part is index 0.
func (s *session) inputTranscript(event serverEvent) ([]provider.RealtimeUpstreamEvent, error) {
	units, err := s.dialect.transcriptionUnits(event)
	if err != nil {
		return nil, err
	}
	charge := func(events []provider.RealtimeUpstreamEvent) []provider.RealtimeUpstreamEvent {
		if len(units) == 0 {
			return events
		}
		if len(events) == 0 {
			return []provider.RealtimeUpstreamEvent{{Units: units}}
		}
		events[len(events)-1].Units = units
		return events
	}
	if !s.transcribingInput() {
		return charge(nil), nil
	}
	if event.ItemID == "" {
		return nil, s.dialect.invalidEvent(event.Type, "item_id is absent")
	}
	events := s.inputRunning(event.ItemID, event.ContentIndex, func(string) string { return event.Transcript })
	s.mu.Lock()
	item := s.inputItem(event.ItemID, event.ContentIndex)
	final := item.committed && !item.done
	if final {
		item.done = true
		item.transcribed = event.Transcript
		item.text = item.ownText()
	}
	transcript := item.text
	s.mu.Unlock()
	if final {
		events = append(events, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptInput, ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: item.contentIndex, Transcript: transcript}})
	}
	return charge(events), nil
}

// relaysAudio is whether a response's audio reaches the client: what it asked
// for, whatever the provider was asked for to give it.
func (s *session) relaysAudio(id contract.RealtimeResponseID) bool {
	asked := s.relayModalities(id)
	return asked == nil || slices.Contains(asked, contract.RealtimeOutputAudio)
}

// outputTranscript is the transcript of a response's speech. For a response
// asked for modalities the wire does not take, the transcript IS the
// response's text: a text-only response's text deltas (its audio is not
// relayed), or a spoken-and-written response's text beside its transcript.
func (s *session) outputTranscript(event serverEvent, done bool) []provider.RealtimeUpstreamEvent {
	responseID := contract.RealtimeResponseID(event.ResponseID)
	item := contract.RealtimeItemID(event.ItemID)
	asked, emulated := s.emulated(responseID)
	var events []provider.RealtimeUpstreamEvent
	if !emulated || slices.Contains(asked, contract.RealtimeOutputAudio) {
		if done {
			events = append(events, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptOutput, ItemID: item, ContentIndex: event.ContentIndex, ResponseID: &responseID, Transcript: event.Transcript}})
		} else {
			events = append(events, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptOutput, ItemID: item, ContentIndex: event.ContentIndex, ResponseID: &responseID, Text: event.Delta}})
		}
	}
	if emulated && slices.Contains(asked, contract.RealtimeOutputText) && !done && event.Delta != "" {
		events = append(events, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeTextDeltaEvent{ResponseID: responseID, ItemID: item, ContentIndex: event.ContentIndex, Text: event.Delta}})
	}
	return events
}

// inputTranscriptionFailed reports that one turn could not be transcribed. It
// refuses nothing the caller sent and leaves the conversation running, so it
// is a non-fatal error naming no command, and it is not the session's own
// failure.
func (s *session) inputTranscriptionFailed(event serverEvent) ([]provider.RealtimeUpstreamEvent, error) {
	if !s.transcribingInput() {
		return nil, nil
	}
	reported := wireError{}
	if event.Error != nil {
		reported = *event.Error
	}
	failure := provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
		Detail: s.dialect.name + " could not transcribe the caller's speech for one turn", Passthrough: s.dialect.passthroughFor(reported, s.key)}
	return one(&contract.RealtimeErrorEvent{Fatal: false, Error: *failure.ContractError(s.requestID)}), nil
}

func itemID(value *string) *contract.RealtimeItemID {
	if value == nil || *value == "" {
		return nil
	}
	id := contract.RealtimeItemID(*value)
	return &id
}

// errorEvent reports an in-band error without ending the session: OpenAI
// states most are refusals of one client event and the session stays open.
// When the connection then ends, a failure about the session itself is what
// the session reports it ended with.
func (s *session) errorEvent(value wireError) []provider.RealtimeUpstreamEvent {
	failure := s.dialect.classifyEvent(s.dialect, value, s.key)
	var upstream provider.ErrUpstream
	if !errors.As(failure, &upstream) {
		upstream = provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: s.dialect.name + "'s Realtime session failed"}
	}
	var commandID *contract.RealtimeCommandID
	s.mu.Lock()
	if upstream.Category != contract.UpstreamInvalidReq {
		s.failure = failure
	}
	if value.EventID != nil && *value.EventID != "" && *value.EventID != openEventID && len(*value.EventID) <= 128 {
		id := contract.RealtimeCommandID(*value.EventID)
		commandID = &id
		for index, pending := range s.pending {
			if pending.commandID == id {
				s.pending = append(s.pending[:index], s.pending[index+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	return []provider.RealtimeUpstreamEvent{{Event: &contract.RealtimeErrorEvent{Fatal: false, Error: *upstream.ContractError(s.requestID)}, CommandID: commandID}}
}

func (s *session) rememberToolName(value item) {
	if value.Type != "function_call" || value.CallID == nil || value.Name == nil {
		return
	}
	s.mu.Lock()
	s.toolNames[*value.CallID] = *value.Name
	s.mu.Unlock()
}

// toolCall streams a function call under the one-shot accumulation rules: the
// name on the first increment, argument text as it arrives, and the complete
// arguments on the last one only when none were streamed before it.
func (s *session) toolCall(event serverEvent, text string, complete bool) contract.RealtimeServerEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := &contract.RealtimeToolCallEvent{ResponseID: contract.RealtimeResponseID(event.ResponseID), ItemID: contract.RealtimeItemID(event.ItemID), ToolCallID: event.CallID, Complete: complete}
	name := s.toolNames[event.CallID]
	if name == "" && event.Name != "" {
		name = event.Name
	}
	if name != "" && !s.namedTool[event.CallID] {
		call.Name = &name
		s.namedTool[event.CallID] = true
	}
	if !complete || !s.streamed[event.CallID] {
		if text != "" || complete {
			call.ArgumentsDelta = &text
		}
	}
	if !complete && text != "" {
		s.streamed[event.CallID] = true
	}
	if complete {
		delete(s.toolNames, event.CallID)
		delete(s.namedTool, event.CallID)
		delete(s.streamed, event.CallID)
	}
	return call
}

// audioFrames splits one output audio delta into contract-bounded frames. The
// split is on decoded bytes, so every frame is independently valid padded
// base64 and the frames concatenate to exactly the audio OpenAI sent.
func (s *session) audioFrames(event serverEvent) ([]provider.RealtimeUpstreamEvent, error) {
	if s.outputFormat == nil {
		return nil, s.dialect.invalidEvent(event.Type, "audio in a session with no output audio format")
	}
	audio, err := base64.StdEncoding.DecodeString(event.Delta)
	if err != nil || len(audio) == 0 {
		return nil, s.dialect.invalidEvent(event.Type, "delta is not base64 audio")
	}
	if s.meter != nil {
		// Measured as it arrives: the provider bills the audio it sent,
		// whether or not the customer receives it.
		s.meter.output(len(audio))
	}
	frames := make([]provider.RealtimeUpstreamEvent, 0, len(audio)/maxAudioFrameBytes+1)
	for start := 0; start < len(audio); start += maxAudioFrameBytes {
		end := min(start+maxAudioFrameBytes, len(audio))
		frames = append(frames, provider.RealtimeUpstreamEvent{Event: &contract.RealtimeOutputAudioDeltaEvent{
			ResponseID: contract.RealtimeResponseID(event.ResponseID), ItemID: contract.RealtimeItemID(event.ItemID),
			ContentIndex: event.ContentIndex, Format: *s.outputFormat, Data: base64.StdEncoding.EncodeToString(audio[start:end]),
		}})
	}
	return frames, nil
}

// responseDone reports a response's end and the units it consumed. A
// cancelled or failed response consumed what it consumed, so its usage is
// carried too; a response with no usage reports none rather than zeros.
func (s *session) responseDone(event serverEvent) ([]provider.RealtimeUpstreamEvent, error) {
	if event.Response == nil || event.Response.ID == "" {
		return nil, s.dialect.invalidEvent(event.Type, "response.id is absent")
	}
	status, known := responseStatus(event.Response.Status)
	if !known {
		return nil, s.dialect.invalidEvent(event.Type, "response.status is not a status this adapter knows")
	}
	reason, err := s.dialect.statusReason(event.Type, *event.Response)
	if err != nil {
		return nil, err
	}
	units, source := []contract.UsageQuantity{}, contract.UsageProviderReported
	switch {
	case !s.dialect.tokenUsage():
		// The provider's token counts are not what it bills (xAI bills audio
		// by the minute), so none is settled; the session's measured units
		// are reported when it closes (meter.go).
		source = contract.UsageOxyMeasured
	case event.Response.Usage != nil:
		measured, consistent := event.Response.Usage.units()
		if !consistent {
			return nil, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
				Detail: s.dialect.name + " reported response usage this adapter cannot partition", Passthrough: &contract.ProviderErrorPassthrough{Provider: s.dialect.slug}}
		}
		units = measured
	}
	id := contract.RealtimeResponseID(event.Response.ID)
	s.mu.Lock()
	if s.openResponse == id {
		s.openResponse = ""
	}
	// A response the provider ended itself restarts a transcriber that
	// continues interrupted turns; one it dropped (endedUnannounced) does not.
	s.responseEnded = true
	s.mu.Unlock()
	done := &contract.RealtimeResponseDoneEvent{
		ResponseID: id, Status: status,
		FinishReason: finishReason(*event.Response, status, reason), Units: units, UsageSource: source,
	}
	return []provider.RealtimeUpstreamEvent{{Event: done, Units: units}}, nil
}

// endedUnannounced is the response.done of a response the provider ended
// without one: cancelled, with no usage of its own. xAI bills by the minute and
// its session is metered (meter.go), so nothing is lost; a provider that
// reports tokens always sends its response.done and never reaches here.
func (s *session) endedUnannounced(id contract.RealtimeResponseID) *contract.RealtimeResponseDoneEvent {
	source := contract.UsageProviderReported
	if !s.dialect.tokenUsage() {
		source = contract.UsageOxyMeasured
	}
	finish := contract.FinishCancelled
	return &contract.RealtimeResponseDoneEvent{
		ResponseID: id, Status: contract.RealtimeResponseCancelled, FinishReason: &finish,
		Units: []contract.UsageQuantity{}, UsageSource: source,
	}
}

// contractItem is an OpenAI conversation item in the contract's shape, or
// false when the contract cannot carry it faithfully.
func (s *session) contractItem(value item) (contract.RealtimeConversationItem, bool) {
	if value.ID == nil || *value.ID == "" {
		return contract.RealtimeConversationItem{}, false
	}
	id := contract.RealtimeItemID(*value.ID)
	converted := contract.RealtimeConversationItem{Type: contract.RealtimeItemType(value.Type), ItemID: &id}
	switch value.Type {
	case "message":
		if value.Role == nil {
			return converted, false
		}
		role := contract.RealtimeItemRole(*value.Role)
		converted.Role = &role
		for _, part := range value.Content {
			switch s.dialect.contractPartType(part.Type, *value.Role) {
			case "input_text", "output_text":
				text := ""
				if part.Text != nil {
					text = *part.Text
				}
				converted.Content = append(converted.Content, contract.RealtimeContentPart{Type: contract.RealtimeContentPartType(s.dialect.contractPartType(part.Type, *value.Role)), Text: &text})
			case "input_audio":
				format := s.inputFormat
				converted.Content = append(converted.Content, contract.RealtimeContentPart{Type: contract.RealtimeInputAudioPart, Format: &format, Transcript: part.Transcript})
			case "output_audio":
				if s.outputFormat == nil {
					return converted, false
				}
				format := *s.outputFormat
				converted.Content = append(converted.Content, contract.RealtimeContentPart{Type: contract.RealtimeOutputAudioPart, Format: &format, Transcript: part.Transcript})
			default:
				return converted, false
			}
		}
	case "function_call":
		arguments := ""
		if value.Arguments != nil {
			arguments = *value.Arguments
		}
		converted.CallID, converted.Name, converted.Arguments = value.CallID, value.Name, &arguments
	case "function_call_output":
		converted.CallID, converted.Output = value.CallID, value.Output
	default:
		return converted, false
	}
	if converted.Validate() != nil {
		return converted, false
	}
	return converted, true
}
