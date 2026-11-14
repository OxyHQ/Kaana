package openairealtime

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// A `live` session on an OpenAI-Realtime-shaped wire is emulated (Kaana ADR
// 0012 §2): neither provider hands a turn to its caller, so the session is a
// conversation whose one tool, `delegate`, is the handing over. The voice
// calls it for every request; Kaana turns the completed call into
// delegation.requested, holds the caller's reply until it is final, and sends
// it back as the call's function_call_output followed by response.create, so
// the voice speaks it. Both dialects serve it the same way: nothing here is a
// provider's, and what the wires spell differently is the dialect's, as for a
// conversation.

// delegateToolName is the one tool a live session's voice is given.
const delegateToolName = "delegate"

// delegateDescription is how the tool is described to the voice. The shared
// preamble (provider.LivePreamble) says every request is delegated; this
// says how on this wire, what its result holds, and whether the voice may
// acknowledge a turn before it calls the tool (config.delegation).
func delegateDescription(acknowledgement contract.RealtimeDelegationAcknowledgement) string {
	before := "Before calling it you may say one very short acknowledgement, such as \"one moment\", and nothing else."
	if acknowledgement == contract.RealtimeAcknowledgementNone {
		before = "Call it without saying anything first."
	}
	return "Hands the person's request over to be answered. Call it for every request the person makes, with the request in the person's own words. " +
		before + " Its result is the answer to speak. A result that begins with \"" + LiveContextLabel + "\" holds context for you to know and never say, " +
		"and the answer to speak follows it after a blank line. A result saying the request was dropped is never spoken."
}

// LiveContextLabel opens a function output that carries delegation.context.
const LiveContextLabel = "Context, not to be said:"

// LiveDroppedOutput is the output a cancelled delegation's call is given: it
// closes the call so the voice does not wait on it, and no response.create
// follows it, so nothing is spoken for it.
const LiveDroppedOutput = "This request was dropped. Say nothing about it."

// delegateTool is the delegate tool definition.
func delegateTool(acknowledgement contract.RealtimeDelegationAcknowledgement) contract.RealtimeToolDefinition {
	description := delegateDescription(acknowledgement)
	return contract.RealtimeToolDefinition{
		Type: "function", Name: delegateToolName, Description: &description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"request": map[string]any{"type": "string", "description": "The person's request, in their own words."},
			},
			"required": []string{"request"},
		},
	}
}

// liveConversation is a live session's configuration as the conversation this
// wire holds: the preamble before the caller's persona, the delegate tool
// under tool choice auto, spoken answers, the caller's speech transcribed (in
// the caller's language) because delegation needs its words, and a turn
// detection that answers every turn and lets the caller interrupt, because
// the voice owns turn-taking. What the wire cannot serve of the caller's own
// configuration is refused first, exactly as Unservable says.
func (d *dialect) liveConversation(config contract.RealtimeSessionConfig) (contract.RealtimeSessionConfig, error) {
	if err := d.refuseUnservable("config", config.Fields()); err != nil {
		return config, err
	}
	acknowledgement := contract.RealtimeAcknowledgementAuto
	if config.Delegation != nil {
		acknowledgement = config.Delegation.Acknowledgement
	}
	served := config
	served.Delegation = nil
	instructions := provider.LiveInstructions(config.Instructions)
	served.Instructions = &instructions
	served.OutputModalities = []contract.RealtimeOutputModality{contract.RealtimeOutputAudio}
	served.Tools = []contract.RealtimeToolDefinition{delegateTool(acknowledgement)}
	auto := contract.ToolChoiceAuto
	served.ToolChoice = &contract.ToolChoice{Mode: &auto}
	transcription := contract.RealtimeInputTranscription{}
	if config.InputAudioTranscription != nil {
		transcription.Language = config.InputAudioTranscription.Language
	}
	served.InputAudioTranscription = &transcription
	answers := true
	served.TurnDetection.CreateResponse, served.TurnDetection.InterruptResponse = &answers, &answers
	return served, nil
}

// liveState is a live session's delegation, as the adapter emulates it.
// Guarded by the session's mu.
type liveState struct {
	// heard is the caller's final transcripts since the last delegation was
	// handed over: what the next one's text is made of.
	heard []string
	// held is a delegate call the voice completed while the caller's turn was
	// still being transcribed: it is handed over once no committed item waits
	// for its final transcript, so its text is the whole turn.
	held *heldDelegation
	// handedOver is the committed items of the caller's speech still without
	// a final transcript when a delegation was handed over without them: no
	// later delegation waits for them, and their words, when they come, are
	// relayed but belong to none.
	handedOver map[string]bool
	// answer is the reply and context so far of the delegation the session
	// last sent a command for (answerFor). It is never reset when a newer
	// delegation is read: the session may still apply the open one's final
	// reply before it supersedes it.
	answer liveAnswer
}

type heldDelegation struct {
	id contract.RealtimeDelegationID
	// request is the voice's own `request` argument: the text when no
	// transcript of the turn arrived.
	request string
}

type liveAnswer struct {
	id      contract.RealtimeDelegationID
	context []string
	reply   strings.Builder
	// units is the reply's length so far in UTF-16 code units, the unit the
	// contract bounds a function output in.
	units int
}

// translateLive is translate for a live session: the delegate call becomes
// delegation.requested, the caller's final transcripts are gathered for it,
// and only what a live session relays reaches the client (liveRelayed).
func (s *session) translateLive(event serverEvent) ([]provider.RealtimeUpstreamEvent, error) {
	var events []provider.RealtimeUpstreamEvent
	release := false
	switch event.Type {
	case "response.function_call_arguments.delta":
		// The call is handed over whole, once complete.
		return nil, nil
	case "response.function_call_arguments.done":
		earlier, err := s.delegated(event)
		if err != nil {
			return nil, err
		}
		if earlier != nil {
			events = append(events, provider.RealtimeUpstreamEvent{Event: earlier})
		}
	case "conversation.item.input_audio_transcription.failed":
		// A turn that will never be transcribed is not waited for.
		s.mu.Lock()
		if item := s.inputs[event.ItemID]; item != nil {
			item.done = true
		}
		delete(s.live.handedOver, event.ItemID)
		s.mu.Unlock()
	case "input_audio_buffer.speech_started":
		// The caller goes on talking: a held delegation is handed over with
		// the words heard so far rather than waiting into the next turn.
		release = true
	}
	translated, err := s.translate(event)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, upstream := range translated {
		if relayed, ok := s.liveRelayed(upstream); ok {
			events = append(events, relayed)
		}
	}
	if s.live.held != nil && (release || !s.awaitingTranscript()) {
		events = append(events, provider.RealtimeUpstreamEvent{Event: s.handOver()})
	}
	return events, nil
}

// delegated holds the voice's completed delegate call until it can be handed
// over; a call to any other name is not the voice's to make and is dropped. A
// call held before it is handed over now, with what was heard, and returned:
// the session then supersedes it with this one.
func (s *session) delegated(event serverEvent) (*contract.RealtimeDelegationRequestedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := event.Name
	if remembered := s.toolNames[event.CallID]; remembered != "" {
		name = remembered
	}
	delete(s.toolNames, event.CallID)
	if name != delegateToolName {
		return nil, nil
	}
	if event.CallID == "" || len(utf16.Encode([]rune(event.CallID))) > 128 {
		return nil, s.dialect.invalidEvent(event.Type, "call_id is not a delegation id")
	}
	var arguments struct {
		Request string `json:"request"`
	}
	// Arguments the voice wrote badly still hand the turn over: its text is
	// the caller's transcript, and only without one the voice's request.
	_ = json.Unmarshal([]byte(event.Arguments), &arguments)
	var earlier *contract.RealtimeDelegationRequestedEvent
	if s.live.held != nil {
		earlier = s.handOver()
	}
	s.live.held = &heldDelegation{id: contract.RealtimeDelegationID(event.CallID), request: strings.TrimSpace(arguments.Request)}
	return earlier, nil
}

// awaitingTranscript reports a committed item of the caller's speech whose
// final transcript has not arrived. Called with s.mu held.
func (s *session) awaitingTranscript() bool {
	for _, id := range s.awaiting {
		if item := s.inputs[id]; item != nil && !item.done && !s.live.handedOver[id] {
			return true
		}
	}
	return false
}

// handOver is the held delegation as delegation.requested. A committed item
// still waiting for its transcript is handed over with it, as words not
// heard: nothing waits for it again. Called with s.mu held.
func (s *session) handOver() *contract.RealtimeDelegationRequestedEvent {
	held := s.live.held
	text := strings.Join(s.live.heard, " ")
	if text == "" {
		text = held.request
	}
	for _, id := range s.awaiting {
		if item := s.inputs[id]; item != nil && !item.done {
			if s.live.handedOver == nil {
				s.live.handedOver = make(map[string]bool)
			}
			s.live.handedOver[id] = true
		}
	}
	s.live.held, s.live.heard = nil, nil
	return &contract.RealtimeDelegationRequestedEvent{DelegationID: held.id, Text: text}
}

// liveRelayed is what of one translated event a live session relays: the
// caller's and the voice's transcripts, the voice's audio and errors, as for a
// conversation, and nothing of the conversation the emulation runs on
// (responses, items, the delegate call, speech and commit markers, the end of
// a response's audio), which GPT-Live has no counterpart of. The voice's
// transcript never carries the verbatim marks. A dropped event's units are
// kept: what the provider billed is settled whatever is relayed. Called with
// s.mu held.
func (s *session) liveRelayed(upstream provider.RealtimeUpstreamEvent) (provider.RealtimeUpstreamEvent, bool) {
	switch e := upstream.Event.(type) {
	case *contract.RealtimeTranscriptDeltaEvent:
		if e.Source == contract.RealtimeTranscriptOutput {
			if e.Text = provider.StripVerbatimMarks(e.Text); e.Text == "" {
				return s.unitsOnly(upstream)
			}
		}
		return upstream, true
	case *contract.RealtimeTranscriptDoneEvent:
		if e.Source == contract.RealtimeTranscriptOutput {
			e.Transcript = provider.StripVerbatimMarks(e.Transcript)
		} else if s.live.handedOver[string(e.ItemID)] {
			delete(s.live.handedOver, string(e.ItemID))
		} else if heard := strings.TrimSpace(e.Transcript); heard != "" {
			s.live.heard = append(s.live.heard, heard)
		}
		return upstream, true
	case *contract.RealtimeOutputAudioDeltaEvent, *contract.RealtimeErrorEvent, nil:
		return upstream, true
	}
	return s.unitsOnly(upstream)
}

// unitsOnly is a dropped event's units, if it carried any.
func (s *session) unitsOnly(upstream provider.RealtimeUpstreamEvent) (provider.RealtimeUpstreamEvent, bool) {
	if len(upstream.Units) == 0 {
		return provider.RealtimeUpstreamEvent{}, false
	}
	return provider.RealtimeUpstreamEvent{Units: upstream.Units}, true
}

// liveCommands is the client events one command of a live session becomes.
// The caller's audio is sent as in a conversation. A delegation's reply is
// held until its final part, then sent as the delegate call's output, the
// context received before it first, and a response.create, so the voice
// speaks it; its cancel closes the call with an output that is not spoken.
// Every other command is the conversation's the emulation runs on, which a
// live session does not expose.
func (s *session) liveCommands(command contract.RealtimeCommand) ([]clientEvent, error) {
	_, commandID := command.Identity()
	switch c := command.(type) {
	case *contract.RealtimeInputAudioAppendCommand:
		event, err := s.wireCommand(c)
		return []clientEvent{event}, err
	case *contract.RealtimeDelegationContextCommand:
		s.mu.Lock()
		defer s.mu.Unlock()
		answer := s.answerFor(c.DelegationID)
		answer.context = append(answer.context, provider.StripVerbatimMarks(c.Text))
		return nil, nil
	case *contract.RealtimeDelegationReplyCommand:
		output, final, err := s.addReply(c)
		if err != nil || !final {
			return nil, err
		}
		return s.callOutput(commandID, c.DelegationID, output, true)
	case *contract.RealtimeDelegationCancelCommand:
		s.mu.Lock()
		s.live.answer = liveAnswer{}
		s.mu.Unlock()
		return s.callOutput(commandID, c.DelegationID, LiveDroppedOutput, false)
	}
	return nil, refused("type", "a live session takes no "+string(command.CommandType()))
}

// answerFor is the answer being gathered for a delegation. The session sends
// commands only for the newest delegation, which is the one handed over last
// (handOver), so another id starts over rather than mixing two answers.
// Called with s.mu held.
func (s *session) answerFor(id contract.RealtimeDelegationID) *liveAnswer {
	if s.live.answer.id != id {
		s.live.answer = liveAnswer{id: id}
	}
	return &s.live.answer
}

// addReply adds one part of a reply, its verbatim spans marked, and on the
// final part returns the whole function output. A reply longer than a
// function output can carry is refused, and the parts before it stay.
func (s *session) addReply(c *contract.RealtimeDelegationReplyCommand) (string, bool, error) {
	part := provider.WrapVerbatim(c.Text, c.Verbatim)
	s.mu.Lock()
	defer s.mu.Unlock()
	answer := s.answerFor(c.DelegationID)
	if answer.reply.Len() > 0 && !endsInSpace(answer.reply.String()) && !startsWithSpace(part) {
		part = " " + part
	}
	units := len(utf16.Encode([]rune(part)))
	context := strings.Join(answer.context, "\n")
	framing := 0
	if context != "" {
		framing = len(utf16.Encode([]rune(LiveContextLabel + " " + context + "\n\n")))
	}
	if framing+answer.units+units > contract.MaxRealtimeItemTextChars {
		return "", false, refused("text", "the reply is longer than one answer can carry")
	}
	answer.reply.WriteString(part)
	answer.units += units
	if !c.Final {
		return "", false, nil
	}
	output := answer.reply.String()
	if context != "" {
		output = LiveContextLabel + " " + context + "\n\n" + output
	}
	s.live.answer = liveAnswer{}
	return output, true, nil
}

func endsInSpace(text string) bool {
	last, _ := utf8.DecodeLastRuneInString(text)
	return unicode.IsSpace(last)
}

func startsWithSpace(text string) bool {
	first, _ := utf8.DecodeRuneInString(text)
	return unicode.IsSpace(first)
}

// callOutput is the delegate call's function_call_output, and, when it is to
// be spoken, the response.create that makes the voice speak it. Both are the
// conversation's own client events, written as a conversation writes them.
func (s *session) callOutput(commandID contract.RealtimeCommandID, delegation contract.RealtimeDelegationID, output string, speak bool) ([]clientEvent, error) {
	callID := string(delegation)
	item, err := s.wireCommand(&contract.RealtimeItemCreateCommand{CommandID: commandID, Item: contract.RealtimeConversationItem{
		Type: contract.RealtimeFunctionCallOutputItem, CallID: &callID, Output: &output,
	}})
	if err != nil || !speak {
		return []clientEvent{item}, err
	}
	respond, err := s.wireCommand(&contract.RealtimeResponseCreateCommand{CommandID: commandID})
	return []clientEvent{item, respond}, err
}
