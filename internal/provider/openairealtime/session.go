package openairealtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

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

	closeOnce sync.Once
}

func newSession(d *dialect, conn *websocket.Conn, request provider.RealtimeOpenRequest, key provider.Key, inputFormat contract.RealtimeAudioFormat) *session {
	return &session{
		dialect: d, conn: conn, requestID: request.RequestID, key: key,
		inputFormat: inputFormat, outputFormat: request.Config.OutputAudioFormat, config: request.Config,
		toolNames: make(map[string]string), namedTool: make(map[string]bool), streamed: make(map[string]bool),
	}
}

// Send applies one command upstream, once.
func (s *session) Send(ctx context.Context, command contract.RealtimeCommand) error {
	event, err := s.wireCommand(command)
	if err != nil {
		return err
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

// sessionModalities is the output modalities in effect for the next response:
// the confirmed configuration, with every update sent since applied in order,
// as the provider applies client events in order.
func (s *session) sessionModalities() []contract.RealtimeOutputModality {
	s.mu.Lock()
	defer s.mu.Unlock()
	modalities := s.config.OutputModalities
	for _, pending := range s.pending {
		if pending.update.OutputModalities != nil {
			modalities = pending.update.OutputModalities
		}
	}
	return modalities
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
			return provider.RealtimeUpstreamEvent{}, s.dialect.invalidEvent()
		}
		var event serverEvent
		if json.Unmarshal(data, &event) != nil {
			return provider.RealtimeUpstreamEvent{}, s.dialect.invalidEvent()
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
	s.closeOnce.Do(func() { err = s.conn.Close(websocket.StatusNormalClosure, "") })
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
			return nil, s.dialect.invalidEvent()
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
			return nil, s.dialect.invalidEvent()
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
		return one(&contract.RealtimeSpeechStartedEvent{ItemID: contract.RealtimeItemID(event.ItemID), AudioStartMs: event.AudioStartMs}), nil
	case "input_audio_buffer.speech_stopped":
		return one(&contract.RealtimeSpeechStoppedEvent{ItemID: contract.RealtimeItemID(event.ItemID), AudioEndMs: event.AudioEndMs}), nil
	case "input_audio_buffer.committed":
		return one(&contract.RealtimeInputAudioCommittedEvent{ItemID: contract.RealtimeItemID(event.ItemID), PreviousItemID: itemID(event.PreviousItemID)}), nil
	case "input_audio_buffer.cleared":
		return one(&contract.RealtimeInputAudioClearedEvent{}), nil

	case "response.created":
		if event.Response == nil || event.Response.ID == "" {
			return nil, s.dialect.invalidEvent()
		}
		return one(&contract.RealtimeResponseCreatedEvent{ResponseID: contract.RealtimeResponseID(event.Response.ID)}), nil
	case "response.output_item.added", "response.output_item.done":
		if event.Item != nil {
			s.rememberToolName(*event.Item)
		}
		return nil, nil

	case "response.output_audio.delta":
		return s.audioFrames(event)
	case "response.output_audio.done":
		return one(&contract.RealtimeOutputAudioDoneEvent{ResponseID: contract.RealtimeResponseID(event.ResponseID), ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex}), nil
	case "response.output_audio_transcript.delta":
		responseID := contract.RealtimeResponseID(event.ResponseID)
		return one(&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptOutput, ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex, ResponseID: &responseID, Text: event.Delta}), nil
	case "response.output_audio_transcript.done":
		responseID := contract.RealtimeResponseID(event.ResponseID)
		return one(&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptOutput, ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex, ResponseID: &responseID, Transcript: event.Transcript}), nil
	case "response.output_text.delta":
		return one(&contract.RealtimeTextDeltaEvent{ResponseID: contract.RealtimeResponseID(event.ResponseID), ItemID: contract.RealtimeItemID(event.ItemID), ContentIndex: event.ContentIndex, Text: event.Delta}), nil

	case "response.function_call_arguments.delta":
		return one(s.toolCall(event, event.Delta, false)), nil
	case "response.function_call_arguments.done":
		return one(s.toolCall(event, event.Arguments, true)), nil

	case "response.done":
		return s.responseDone(event)
	}
	// session.created after open, conversation.created, rate_limits.updated,
	// content parts, output_text.done (the deltas already carried it),
	// input_audio_buffer.timeout_triggered, WebRTC-only output buffer events,
	// MCP events: nothing in the contract names them.
	return nil, nil
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
		return nil, s.dialect.invalidEvent()
	}
	audio, err := base64.StdEncoding.DecodeString(event.Delta)
	if err != nil || len(audio) == 0 {
		return nil, s.dialect.invalidEvent()
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
		return nil, s.dialect.invalidEvent()
	}
	status, known := responseStatus(event.Response.Status)
	if !known {
		return nil, s.dialect.invalidEvent()
	}
	units, source := []contract.UsageQuantity{}, contract.UsageProviderReported
	switch {
	case !s.dialect.tokenUsage:
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
	done := &contract.RealtimeResponseDoneEvent{
		ResponseID: contract.RealtimeResponseID(event.Response.ID), Status: status,
		FinishReason: finishReason(*event.Response, status), Units: units, UsageSource: source,
	}
	return []provider.RealtimeUpstreamEvent{{Event: done, Units: units}}, nil
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
