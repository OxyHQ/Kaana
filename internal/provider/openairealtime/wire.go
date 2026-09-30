package openairealtime

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// OpenAI's GA Realtime wire, as far as this adapter speaks it, and xAI's
// Voice Agent dialect of it (dialect.go). Reviewed against
// https://developers.openai.com/api/reference/resources/realtime/client-events,
// .../server-events and https://docs.x.ai/voice-realtime.ws.json on
// 2026-09-30. Field names are the providers'; nothing here is a contract shape.

type audioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate,omitempty"`
}

type audioInput struct {
	Format *audioFormat `json:"format,omitempty"`
	// TurnDetection is raw so that `null` — OpenAI's spelling of "the client
	// commits its own turns" — is sent explicitly rather than dropped.
	TurnDetection json.RawMessage `json:"turn_detection,omitempty"`
}

type audioOutput struct {
	Format *audioFormat `json:"format,omitempty"`
	Voice  *string      `json:"voice,omitempty"`
}

type audioConfig struct {
	Input  *audioInput  `json:"input,omitempty"`
	Output *audioOutput `json:"output,omitempty"`
}

type functionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type sessionConfig struct {
	Type             string          `json:"type,omitempty"`
	OutputModalities []string        `json:"output_modalities,omitempty"`
	Instructions     *string         `json:"instructions,omitempty"`
	Voice            *string         `json:"voice,omitempty"`
	TurnDetection    json.RawMessage `json:"turn_detection,omitempty"`
	Audio            *audioConfig    `json:"audio,omitempty"`
	Tools            []functionTool  `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	MaxOutputTokens  *int            `json:"max_output_tokens,omitempty"`
}

type contentPart struct {
	Type       string  `json:"type"`
	Text       *string `json:"text,omitempty"`
	Audio      *string `json:"audio,omitempty"`
	Transcript *string `json:"transcript,omitempty"`
}

type item struct {
	ID        *string       `json:"id,omitempty"`
	Type      string        `json:"type"`
	Role      *string       `json:"role,omitempty"`
	Content   []contentPart `json:"content,omitempty"`
	CallID    *string       `json:"call_id,omitempty"`
	Name      *string       `json:"name,omitempty"`
	Arguments *string       `json:"arguments,omitempty"`
	Output    *string       `json:"output,omitempty"`
}

type responseParameters struct {
	Instructions     *string         `json:"instructions,omitempty"`
	OutputModalities []string        `json:"output_modalities,omitempty"`
	Modalities       []string        `json:"modalities,omitempty"`
	MaxOutputTokens  *int            `json:"max_output_tokens,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
}

// clientEvent is every client event this adapter sends. event_id carries the
// contract commandId, which OpenAI echoes on the error a command causes and on
// nothing else.
type clientEvent struct {
	Type           string              `json:"type"`
	EventID        string              `json:"event_id,omitempty"`
	Session        *sessionConfig      `json:"session,omitempty"`
	Audio          *string             `json:"audio,omitempty"`
	PreviousItemID *string             `json:"previous_item_id,omitempty"`
	Item           *item               `json:"item,omitempty"`
	ItemID         *string             `json:"item_id,omitempty"`
	ContentIndex   *int                `json:"content_index,omitempty"`
	AudioEndMs     *int                `json:"audio_end_ms,omitempty"`
	Response       *responseParameters `json:"response,omitempty"`
	ResponseID     *string             `json:"response_id,omitempty"`
}

type wireError struct {
	Type    string  `json:"type"`
	Code    *string `json:"code"`
	Message string  `json:"message"`
	EventID *string `json:"event_id"`
}

type tokenDetails struct {
	TextTokens  int `json:"text_tokens"`
	AudioTokens int `json:"audio_tokens"`
	ImageTokens int `json:"image_tokens"`
}

type inputTokenDetails struct {
	tokenDetails
	CachedTokens        int          `json:"cached_tokens"`
	CachedTokensDetails tokenDetails `json:"cached_tokens_details"`
}

type usage struct {
	TotalTokens        *int              `json:"total_tokens"`
	InputTokens        int               `json:"input_tokens"`
	OutputTokens       int               `json:"output_tokens"`
	InputTokenDetails  inputTokenDetails `json:"input_token_details"`
	OutputTokenDetails tokenDetails      `json:"output_token_details"`
}

type response struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// StatusDetails is raw because its shape is the dialect's: OpenAI's is an
	// object or null, xAI's the string "unimplemented" (statusReason).
	StatusDetails json.RawMessage `json:"status_details"`
	Output        []item          `json:"output"`
	Usage         *usage          `json:"usage"`
}

// serverEvent is the union of every server event field this adapter reads.
type serverEvent struct {
	Type           string     `json:"type"`
	Error          *wireError `json:"error"`
	Item           *item      `json:"item"`
	PreviousItemID *string    `json:"previous_item_id"`
	ItemID         string     `json:"item_id"`
	ContentIndex   int        `json:"content_index"`
	AudioStartMs   int        `json:"audio_start_ms"`
	AudioEndMs     int        `json:"audio_end_ms"`
	ResponseID     string     `json:"response_id"`
	Response       *response  `json:"response"`
	Delta          string     `json:"delta"`
	Transcript     string     `json:"transcript"`
	CallID         string     `json:"call_id"`
	Name           string     `json:"name"`
	Arguments      string     `json:"arguments"`
}

/* -------------------------------------------------------------------------- */
/*  Contract -> provider                                                      */
/* -------------------------------------------------------------------------- */

func refused(param, detail string) error {
	return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: detail}
}

// wireFormat names an audio encoding the way OpenAI does. PCM16 is 24 kHz
// only, which is exactly what the contract's pcm16_24khz says.
func wireFormat(format contract.RealtimeAudioFormat) (*audioFormat, error) {
	switch format {
	case contract.RealtimePCM16:
		return &audioFormat{Type: "audio/pcm", Rate: 24_000}, nil
	case contract.RealtimeULaw:
		return &audioFormat{Type: "audio/pcmu"}, nil
	case contract.RealtimeALaw:
		return &audioFormat{Type: "audio/pcma"}, nil
	}
	return nil, fmt.Errorf("openairealtime: %q is not an audio format", format)
}

func (d *dialect) wireTurnDetection(detection contract.RealtimeTurnDetection) (json.RawMessage, error) {
	if reason, refusedHere := d.refusedTurnDetection[detection.Type]; refusedHere {
		return nil, refused("config.turnDetection", reason)
	}
	switch detection.Type {
	case contract.TurnDetectionNone:
		return d.turnDetectionNone, nil
	case contract.TurnDetectionServerVAD:
		if !d.vad.responseFields {
			return d.wireFixedServerVAD(detection)
		}
		return json.Marshal(struct {
			Type              string   `json:"type"`
			Threshold         *float64 `json:"threshold,omitempty"`
			PrefixPaddingMs   *int     `json:"prefix_padding_ms,omitempty"`
			SilenceDurationMs *int     `json:"silence_duration_ms,omitempty"`
			CreateResponse    bool     `json:"create_response"`
			InterruptResponse bool     `json:"interrupt_response"`
		}{"server_vad", detection.Threshold, detection.PrefixPaddingMs, detection.SilenceDurationMs, *detection.CreateResponse, *detection.InterruptResponse})
	case contract.TurnDetectionSemanticVAD:
		return json.Marshal(struct {
			Type              string `json:"type"`
			Eagerness         string `json:"eagerness"`
			CreateResponse    bool   `json:"create_response"`
			InterruptResponse bool   `json:"interrupt_response"`
		}{"semantic_vad", string(*detection.Eagerness), *detection.CreateResponse, *detection.InterruptResponse})
	}
	return nil, refused("config.turnDetection", "the turn detection is not one "+d.name+"'s Realtime API names")
}

// wireFixedServerVAD is server_vad on a provider whose VAD always answers the
// turn it detected and lets the caller's speech interrupt the answer (xAI:
// no create_response or interrupt_response in its session schema; a spoken
// item is `interruptible`, "Default: true"). The contract's two booleans must
// state exactly that behaviour, and the three tunables must be inside the
// provider's own bounds; anything else is refused, never clamped.
func (d *dialect) wireFixedServerVAD(detection contract.RealtimeTurnDetection) (json.RawMessage, error) {
	if detection.CreateResponse == nil || !*detection.CreateResponse {
		return nil, refused("config.turnDetection.createResponse", d.name+"'s server_vad always creates the response to a turn it detected; it documents no create_response")
	}
	if detection.InterruptResponse == nil || !*detection.InterruptResponse {
		return nil, refused("config.turnDetection.interruptResponse", d.name+"'s server_vad lets the caller's speech interrupt a response; it documents no interrupt_response")
	}
	if detection.Threshold != nil && (*detection.Threshold < d.vad.minThreshold || *detection.Threshold > d.vad.maxThreshold) {
		return nil, refused("config.turnDetection.threshold", fmt.Sprintf("%s's server_vad threshold is %.1f to %.1f", d.name, d.vad.minThreshold, d.vad.maxThreshold))
	}
	for _, bound := range []struct {
		param string
		value *int
	}{{"prefixPaddingMs", detection.PrefixPaddingMs}, {"silenceDurationMs", detection.SilenceDurationMs}} {
		if bound.value != nil && d.vad.maxDurationMs > 0 && *bound.value > d.vad.maxDurationMs {
			return nil, refused("config.turnDetection."+bound.param, fmt.Sprintf("%s's server_vad takes at most %d ms", d.name, d.vad.maxDurationMs))
		}
	}
	return json.Marshal(struct {
		Type              string   `json:"type"`
		Threshold         *float64 `json:"threshold,omitempty"`
		PrefixPaddingMs   *int     `json:"prefix_padding_ms,omitempty"`
		SilenceDurationMs *int     `json:"silence_duration_ms,omitempty"`
	}{"server_vad", detection.Threshold, detection.PrefixPaddingMs, detection.SilenceDurationMs})
}

// wireModalities maps the contract's output modalities. OpenAI's GA schema
// takes exactly one: ["audio"] (speech with its transcript) or ["text"]. The
// contract's ["text","audio"] asks for written output AND speech, which OpenAI
// cannot produce in one response, so it is refused rather than narrowed. xAI's
// response.create takes any of `text` and `audio`, but xAI answers aloud
// whatever it is sent (dialect.alwaysSpeaks), so there a set without audio is
// refused: at open, on a session.update and on a response.create alike.
func (d *dialect) wireModalities(param string, modalities []contract.RealtimeOutputModality) ([]string, error) {
	if modalities == nil {
		return nil, nil
	}
	if d.oneModality && len(modalities) != 1 {
		return nil, refused(param, d.name+"'s Realtime API produces either text or audio in one response, never both")
	}
	if d.alwaysSpeaks && !slices.Contains(modalities, contract.RealtimeOutputAudio) {
		return nil, refused(param, d.name+" answers every response aloud whatever modalities it is sent, and bills that audio; a text-only response cannot be served, so include audio (the spoken answer's transcript is relayed beside it)")
	}
	wire := make([]string, 0, len(modalities))
	for _, modality := range modalities {
		wire = append(wire, string(modality))
	}
	return wire, nil
}

// wireTools maps function tools to the flat shape both providers' guides use
// ({type, name, description, parameters}). xAI's machine-readable schema nests
// the definition under `function`, but every example in its guide is flat,
// like OpenAI's; docs/realtime.md records the discrepancy.
func (d *dialect) wireTools(tools []contract.ToolDefinition) ([]functionTool, error) {
	if tools == nil {
		return nil, nil
	}
	wire := make([]functionTool, 0, len(tools))
	for index, tool := range tools {
		if tool.Type != "function" {
			return nil, refused(fmt.Sprintf("config.tools[%d].type", index), "only function tools are served on "+d.name+"'s Realtime API")
		}
		if tool.Strict != nil {
			return nil, refused(fmt.Sprintf("config.tools[%d].strict", index), d.name+"'s Realtime function tool has no strict mode")
		}
		wire = append(wire, functionTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return wire, nil
}

func (d *dialect) wireToolChoice(param string, choice *contract.ToolChoice) (json.RawMessage, error) {
	if choice == nil {
		return nil, nil
	}
	if !d.toolChoice {
		return nil, refused(param, d.name+"'s Realtime API documents no tool_choice")
	}
	if choice.Function != nil {
		return json.Marshal(struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}{"function", choice.Function.Name})
	}
	return json.Marshal(choice)
}

// wireMaxOutputTokens is OpenAI GA's max_output_tokens (1..4096, or "inf");
// xAI documents no output-token ceiling.
func (d *dialect) wireMaxOutputTokens(param string, limit *int) (*int, error) {
	if limit == nil {
		return nil, nil
	}
	if d.maxOutputTokens == 0 {
		return nil, refused(param, d.name+"'s Realtime API documents no output token ceiling")
	}
	if *limit > d.maxOutputTokens {
		return nil, refused(param, fmt.Sprintf("%s's Realtime API takes at most %d output tokens per response", d.name, d.maxOutputTokens))
	}
	return limit, nil
}

// sessionFields is what a full configuration and an update share.
type sessionFields struct {
	instructions            *string
	outputModalities        []contract.RealtimeOutputModality
	turnDetection           *contract.RealtimeTurnDetection
	inputAudioTranscription *contract.RealtimeInputTranscription
	tools                   []contract.ToolDefinition
	toolChoice              *contract.ToolChoice
	temperature             *float64
	maxOutputTokens         *int
}

func (f sessionFields) wire(d *dialect) (*sessionConfig, error) {
	if f.temperature != nil {
		// Removed from OpenAI's GA session and response schemas (it survives
		// only in the beta interface this adapter does not speak), and absent
		// from xAI's.
		return nil, refused("config.temperature", d.name+"'s Realtime API has no temperature")
	}
	if f.inputAudioTranscription != nil {
		// OpenAI runs a second, separately billed ASR model; xAI documents
		// transcription settings but not what enabling `grok-transcribe`
		// costs. Either way the session's units could not keep that charge
		// apart from the session model's.
		return nil, refused("config.inputAudioTranscription", "input transcription runs a second, separately billed model whose charge the contract's session units cannot keep apart from the session model's; it is not served")
	}
	config := &sessionConfig{Type: d.sessionType, Instructions: f.instructions}
	var err error
	modalities, err := d.wireModalities("config.outputModalities", f.outputModalities)
	if err != nil {
		return nil, err
	}
	if d.sessionModalities {
		config.OutputModalities = modalities
	}
	// Otherwise (xAI) the session's modalities have no session field: they
	// ride on every response.create instead (wireCommand).
	if f.turnDetection != nil {
		detection, err := d.wireTurnDetection(*f.turnDetection)
		if err != nil {
			return nil, err
		}
		if d.flatSession {
			config.TurnDetection = detection
		} else {
			config.Audio = &audioConfig{Input: &audioInput{TurnDetection: detection}}
		}
	}
	if config.Tools, err = d.wireTools(f.tools); err != nil {
		return nil, err
	}
	if config.ToolChoice, err = d.wireToolChoice("config.toolChoice", f.toolChoice); err != nil {
		return nil, err
	}
	if config.MaxOutputTokens, err = d.wireMaxOutputTokens("config.maxOutputTokens", f.maxOutputTokens); err != nil {
		return nil, err
	}
	return config, nil
}

// wireSession is the session.update that configures a new conversation.
func (d *dialect) wireSession(config contract.RealtimeSessionConfig) (*sessionConfig, error) {
	if config.Translation != nil {
		return nil, refused("config.translation", "a conversation does not translate")
	}
	turnDetection := config.TurnDetection
	session, err := sessionFields{
		instructions: config.Instructions, outputModalities: config.OutputModalities, turnDetection: &turnDetection,
		inputAudioTranscription: config.InputAudioTranscription, tools: config.Tools, toolChoice: config.ToolChoice,
		temperature: config.Temperature, maxOutputTokens: config.MaxOutputTokens,
	}.wire(d)
	if err != nil {
		return nil, err
	}
	input, err := wireFormat(config.InputAudioFormat)
	if err != nil {
		return nil, refused("config.inputAudioFormat", err.Error())
	}
	if session.Audio == nil {
		session.Audio = &audioConfig{Input: &audioInput{}}
	}
	session.Audio.Input.Format = input
	voice := config.Voice
	if d.flatSession {
		session.Voice, voice = config.Voice, nil
	}
	if config.OutputAudioFormat != nil || voice != nil {
		session.Audio.Output = &audioOutput{Voice: voice}
		if config.OutputAudioFormat != nil {
			if session.Audio.Output.Format, err = wireFormat(*config.OutputAudioFormat); err != nil {
				return nil, refused("config.outputAudioFormat", err.Error())
			}
		}
	}
	return session, nil
}

// wireUpdate is the session.update a contract session.update command becomes.
func (d *dialect) wireUpdate(update contract.RealtimeSessionConfigUpdate) (*sessionConfig, error) {
	return sessionFields{
		instructions: update.Instructions, outputModalities: update.OutputModalities, turnDetection: update.TurnDetection,
		inputAudioTranscription: update.InputAudioTranscription, tools: update.Tools, toolChoice: update.ToolChoice,
		temperature: update.Temperature, maxOutputTokens: update.MaxOutputTokens,
	}.wire(d)
}

// mergeUpdate is the effective configuration once OpenAI confirmed an update.
func mergeUpdate(config contract.RealtimeSessionConfig, update contract.RealtimeSessionConfigUpdate) contract.RealtimeSessionConfig {
	if update.Instructions != nil {
		config.Instructions = update.Instructions
	}
	if update.OutputModalities != nil {
		config.OutputModalities = update.OutputModalities
	}
	if update.TurnDetection != nil {
		config.TurnDetection = *update.TurnDetection
	}
	if update.Tools != nil {
		config.Tools = update.Tools
	}
	if update.ToolChoice != nil {
		config.ToolChoice = update.ToolChoice
	}
	if update.MaxOutputTokens != nil {
		config.MaxOutputTokens = update.MaxOutputTokens
	}
	return config
}

// wireItem is a contract conversation item on OpenAI's wire. An input_audio
// part carries its frame in the session's own input format (OpenAI's part has
// no format of its own), and an assistant's spoken audio cannot be created by
// a client at all.
func (d *dialect) wireItem(value contract.RealtimeConversationItem, inputFormat contract.RealtimeAudioFormat) (*item, error) {
	wire := &item{Type: string(value.Type), CallID: value.CallID, Name: value.Name, Arguments: value.Arguments, Output: value.Output}
	if value.ItemID != nil {
		id := string(*value.ItemID)
		wire.ID = &id
	}
	if value.Role != nil {
		role := string(*value.Role)
		wire.Role = &role
	}
	for index, part := range value.Content {
		param := fmt.Sprintf("item.content[%d]", index)
		switch part.Type {
		case contract.RealtimeInputTextPart:
			wire.Content = append(wire.Content, contentPart{Type: string(part.Type), Text: part.Text})
		case contract.RealtimeOutputTextPart:
			wire.Content = append(wire.Content, contentPart{Type: d.outputTextPart, Text: part.Text})
		case contract.RealtimeInputAudioPart:
			if part.Data == nil {
				return nil, refused(param+".data", d.name+"'s input_audio part carries its audio inline")
			}
			if *part.Format != inputFormat {
				return nil, refused(param+".format", d.name+" reads an input_audio part in the session's input audio format")
			}
			wire.Content = append(wire.Content, contentPart{Type: "input_audio", Audio: part.Data, Transcript: part.Transcript})
		case contract.RealtimeOutputAudioPart:
			return nil, refused(param+".type", d.name+"'s Realtime API does not accept assistant audio from a client")
		}
	}
	return wire, nil
}

// wireCommand is the client event one contract command becomes.
func (s *session) wireCommand(command contract.RealtimeCommand) (clientEvent, error) {
	_, commandID := command.Identity()
	event := clientEvent{EventID: string(commandID)}
	switch c := command.(type) {
	case *contract.RealtimeSessionUpdateCommand:
		session, err := s.dialect.wireUpdate(c.Config)
		if err != nil {
			return event, err
		}
		event.Type, event.Session = "session.update", session
	case *contract.RealtimeItemCreateCommand:
		wire, err := s.dialect.wireItem(c.Item, s.inputFormat)
		if err != nil {
			return event, err
		}
		event.Type, event.Item = "conversation.item.create", wire
		if c.PreviousItemID != nil {
			previous := string(*c.PreviousItemID)
			event.PreviousItemID = &previous
		}
	case *contract.RealtimeItemDeleteCommand:
		id := string(c.ItemID)
		event.Type, event.ItemID = "conversation.item.delete", &id
	case *contract.RealtimeItemTruncateCommand:
		id, index, end := string(c.ItemID), c.ContentIndex, c.AudioEndMs
		event.Type, event.ItemID, event.ContentIndex, event.AudioEndMs = "conversation.item.truncate", &id, &index, &end
	case *contract.RealtimeInputAudioAppendCommand:
		data := c.Data
		event.Type, event.Audio = "input_audio_buffer.append", &data
	case *contract.RealtimeInputAudioCommitCommand:
		event.Type = "input_audio_buffer.commit"
	case *contract.RealtimeInputAudioClearCommand:
		event.Type = "input_audio_buffer.clear"
	case *contract.RealtimeResponseCreateCommand:
		event.Type = "response.create"
		d := s.dialect
		parameters := c.Response
		if parameters == nil && !d.sessionModalities {
			parameters = &contract.RealtimeResponseParameters{}
		}
		if parameters != nil {
			wire := &responseParameters{Instructions: parameters.Instructions}
			requested := parameters.OutputModalities
			if requested == nil && !d.sessionModalities {
				// The session's modalities, carried on the one field the
				// provider has for them. Every response of a push-to-talk
				// session is created here, so none escapes it.
				requested = s.sessionModalities()
			}
			modalities, err := d.wireModalities("response.outputModalities", requested)
			if err != nil {
				return event, err
			}
			if d.responseModalitiesField == "modalities" {
				wire.Modalities = modalities
			} else {
				wire.OutputModalities = modalities
			}
			if wire.MaxOutputTokens, err = d.wireMaxOutputTokens("response.maxOutputTokens", parameters.MaxOutputTokens); err != nil {
				return event, err
			}
			if wire.ToolChoice, err = d.wireToolChoice("response.toolChoice", parameters.ToolChoice); err != nil {
				return event, err
			}
			event.Response = wire
		}
	case *contract.RealtimeResponseCancelCommand:
		event.Type = "response.cancel"
		if c.ResponseID != nil {
			id := string(*c.ResponseID)
			event.ResponseID = &id
		}
	default:
		// session.resume and session.close belong to the session, not to the
		// provider: the data plane answers them itself.
		return event, refused("type", fmt.Sprintf("%s is not sent upstream", command.CommandType()))
	}
	return event, nil
}

/* -------------------------------------------------------------------------- */
/*  provider -> contract                                                      */
/* -------------------------------------------------------------------------- */

// units is response.done's usage in the contract's partition. OpenAI nests
// the other way round: input_tokens INCLUDES cached tokens and audio tokens,
// input_token_details.audio_tokens INCLUDES the cached audio, and
// output_tokens INCLUDES the audio output tokens. Every unit below is
// therefore a subtraction, and a negative result is a report this adapter
// cannot have read correctly, so it is refused rather than clamped.
//
//	cached_audio_input_tokens = input_token_details.cached_tokens_details.audio_tokens
//	audio_input_tokens        = input_token_details.audio_tokens - cached_audio_input_tokens
//	cached_input_tokens       = input_token_details.cached_tokens - cached_audio_input_tokens
//	input_tokens              = input_tokens - cached_tokens - audio_input_tokens
//	audio_output_tokens       = output_token_details.audio_tokens
//	output_tokens             = output_tokens - audio_output_tokens
func (u usage) units() ([]contract.UsageQuantity, bool) {
	details := u.InputTokenDetails
	cachedAudio := details.CachedTokensDetails.AudioTokens
	audioInput := details.AudioTokens - cachedAudio
	cachedInput := details.CachedTokens - cachedAudio
	input := u.InputTokens - details.CachedTokens - audioInput
	audioOutput := u.OutputTokenDetails.AudioTokens
	output := u.OutputTokens - audioOutput
	for _, value := range []int{cachedAudio, audioInput, cachedInput, input, audioOutput, output} {
		if value < 0 {
			return nil, false
		}
	}
	if u.TotalTokens != nil && *u.TotalTokens != u.InputTokens+u.OutputTokens {
		return nil, false
	}
	return []contract.UsageQuantity{
		{Unit: contract.UnitInputTokens, Quantity: input},
		{Unit: contract.UnitCachedInputTokens, Quantity: cachedInput},
		{Unit: contract.UnitAudioInputTokens, Quantity: audioInput},
		{Unit: contract.UnitCachedAudioInputTokens, Quantity: cachedAudio},
		{Unit: contract.UnitOutputTokens, Quantity: output},
		{Unit: contract.UnitAudioOutputTokens, Quantity: audioOutput},
	}, true
}

func responseStatus(status string) (contract.RealtimeResponseStatus, bool) {
	switch status {
	case "completed":
		return contract.RealtimeResponseCompleted, true
	case "cancelled":
		return contract.RealtimeResponseCancelled, true
	case "incomplete":
		return contract.RealtimeResponseIncomplete, true
	case "failed":
		return contract.RealtimeResponseFailed, true
	}
	return "", false
}

// statusReason reads a response's status_details.reason. OpenAI documents
// status_details as an object or null. xAI sends the string "unimplemented"
// on both response.created and response.done (measured 2026-09-30), which
// carries no reason, so on xAI a string is read as no details. Anything else
// is an event this adapter cannot read.
func (d *dialect) statusReason(eventType string, value response) (*string, error) {
	raw := value.StatusDetails
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if d.statusDetailsText {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return nil, nil
		}
	}
	var details struct {
		Reason *string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, d.invalidEvent(eventType, "response.status_details"+unreadableKind(err))
	}
	return details.Reason, nil
}

// finishReason reads status_details.reason, and the output, for why a
// response stopped. A reason the provider did not give is left absent.
func finishReason(value response, status contract.RealtimeResponseStatus, given *string) *contract.FinishReason {
	reason := ""
	if given != nil {
		reason = *given
	}
	var finish contract.FinishReason
	switch {
	case status == contract.RealtimeResponseCompleted:
		finish = contract.FinishStop
		for _, output := range value.Output {
			if output.Type == "function_call" {
				finish = contract.FinishToolCalls
			}
		}
	case status == contract.RealtimeResponseCancelled:
		finish = contract.FinishCancelled
	case reason == "max_output_tokens":
		finish = contract.FinishLength
	case reason == "content_filter":
		finish = contract.FinishContentFilter
	default:
		return nil
	}
	return &finish
}
