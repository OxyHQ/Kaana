package openairealtime

import (
	"encoding/json"
	"fmt"
	"math"
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
	// Transcription is input transcription inside the conversation: OpenAI's
	// `audio.input.transcription {model, language, prompt}` and xAI's
	// `audio.input.transcription {model, language_hint}`.
	Transcription *transcriptionConfig `json:"transcription,omitempty"`
}

type transcriptionConfig struct {
	Model        string  `json:"model"`
	Language     *string `json:"language,omitempty"`
	LanguageHint *string `json:"language_hint,omitempty"`
	Prompt       *string `json:"prompt,omitempty"`
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
	// Tools is a pointer so an emptied tool list is sent as `[]`, which is
	// how a session that withholds its tools clears them upstream.
	Tools           *[]functionTool `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
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
	// Usage is raw because two events carry a top-level `usage` of different
	// shapes: an input transcription's (OpenAI: tokens or duration, read by
	// transcriptionUnits) and xAI's response.done (tokens it does not bill,
	// never read).
	Usage json.RawMessage `json:"usage"`
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

// wireTurnDetection is the contract's turn detection on the provider's wire
// (Unservable has refused what no translation serves): semantic_vad runs as
// server_vad where the wire has no semantic detection, the contract's 0 to 1
// threshold is mapped onto the wire's own range, and a detection with fixed
// responses (xAI's server_vad, which always answers and is interruptible) has
// no create_response or interrupt_response on the wire.
func (d *dialect) wireTurnDetection(detection contract.RealtimeTurnDetection) (json.RawMessage, error) {
	if detection.Type == contract.TurnDetectionNone {
		return d.turnDetectionNone, nil
	}
	served, ok := d.statement().Parameters.WireTurnDetection(detection.Type)
	if !ok {
		return nil, refused("config.turnDetection", "this model cannot detect turns by "+string(detection.Type))
	}
	if served.Type == contract.TurnDetectionSemanticVAD {
		return json.Marshal(struct {
			Type              string `json:"type"`
			Eagerness         string `json:"eagerness"`
			CreateResponse    bool   `json:"create_response"`
			InterruptResponse bool   `json:"interrupt_response"`
		}{"semantic_vad", string(*detection.Eagerness), *detection.CreateResponse, *detection.InterruptResponse})
	}
	threshold := detection.Threshold
	if threshold != nil && served.Threshold != nil {
		mapped := served.Threshold.Map(*threshold)
		threshold = &mapped
	}
	if served.FixedResponses {
		return json.Marshal(struct {
			Type              string   `json:"type"`
			Threshold         *float64 `json:"threshold,omitempty"`
			PrefixPaddingMs   *int     `json:"prefix_padding_ms,omitempty"`
			SilenceDurationMs *int     `json:"silence_duration_ms,omitempty"`
		}{"server_vad", threshold, detection.PrefixPaddingMs, detection.SilenceDurationMs})
	}
	return json.Marshal(struct {
		Type              string   `json:"type"`
		Threshold         *float64 `json:"threshold,omitempty"`
		PrefixPaddingMs   *int     `json:"prefix_padding_ms,omitempty"`
		SilenceDurationMs *int     `json:"silence_duration_ms,omitempty"`
		CreateResponse    bool     `json:"create_response"`
		InterruptResponse bool     `json:"interrupt_response"`
	}{"server_vad", threshold, detection.PrefixPaddingMs, detection.SilenceDurationMs, *detection.CreateResponse, *detection.InterruptResponse})
}

// wireModalities writes the output modalities of a served set.
func wireModalities(modalities []contract.RealtimeOutputModality) []string {
	if modalities == nil {
		return nil
	}
	wire := make([]string, 0, len(modalities))
	for _, modality := range modalities {
		wire = append(wire, string(modality))
	}
	return wire
}

// wireTools maps function tools to the flat shape both providers' guides use
// ({type, name, description, parameters}). xAI's machine-readable schema nests
// the definition under `function`, but every example in its guide is flat,
// like OpenAI's; docs/realtime.md records the discrepancy.
func (d *dialect) wireTools(tools []contract.RealtimeToolDefinition) (*[]functionTool, error) {
	wire := make([]functionTool, 0, len(tools))
	for index, tool := range tools {
		if tool.Type != "function" {
			return nil, refused(fmt.Sprintf("config.tools[%d].type", index), "only function tools are called in a session")
		}
		wire = append(wire, functionTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return &wire, nil
}

// withholdsTools reports a tool choice served by sending the provider no
// tools: `none` on a wire with no tool_choice, which would otherwise call
// them as it saw fit.
func (d *dialect) withholdsTools(choice *contract.ToolChoice) bool {
	return !d.toolChoiceField && choice != nil && contract.RealtimeToolChoiceOf(*choice) == contract.RealtimeToolChoiceNone
}

// wireToolChoice writes a tool choice on a wire that has the field; without
// one, `auto` is the provider's own behaviour and `none` is served by
// withholding the tools (withholdsTools).
func (d *dialect) wireToolChoice(choice *contract.ToolChoice) (json.RawMessage, error) {
	if choice == nil || !d.toolChoiceField {
		return nil, nil
	}
	if choice.Function != nil {
		return json.Marshal(struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}{"function", choice.Function.Name})
	}
	return json.Marshal(choice)
}

// wireMaxOutputTokens is the contract's per-response cap on the wire, at
// most the wire's own (a response cannot be longer than the provider allows
// anyway). Unservable refused one on a wire with no cap.
func (d *dialect) wireMaxOutputTokens(limit *int) *int {
	ceiling := d.statement().Parameters.MaxOutputTokens
	if limit == nil || ceiling == nil || *limit <= *ceiling {
		return limit
	}
	capped := *ceiling
	return &capped
}

// sessionFields is what a full configuration and an update share.
type sessionFields struct {
	instructions            *string
	outputModalities        []contract.RealtimeOutputModality
	turnDetection           *contract.RealtimeTurnDetection
	inputAudioTranscription *contract.RealtimeInputTranscription
	tools                   []contract.RealtimeToolDefinition
	toolChoice              *contract.ToolChoice
	maxOutputTokens         *int
}

func (f sessionFields) governed() contract.RealtimeConfigFields {
	return contract.RealtimeConfigFields{TurnDetection: f.turnDetection, ToolChoice: f.toolChoice, MaxOutputTokens: f.maxOutputTokens}
}

// wire is the fields on the provider's wire. effective is the session's whole
// configuration once these fields apply: a provider that serves `none` by
// withholding the tools is sent the tools the configuration then has, or none.
func (f sessionFields) wire(d *dialect, effective contract.RealtimeSessionConfig) (*sessionConfig, error) {
	if err := d.refuseUnservable("config", f.governed()); err != nil {
		return nil, err
	}
	parameters := d.statement().Parameters
	config := &sessionConfig{Type: d.sessionType, Instructions: f.instructions, MaxOutputTokens: d.wireMaxOutputTokens(f.maxOutputTokens)}
	if d.sessionModalities {
		config.OutputModalities = wireModalities(parameters.WireOutputModalities(f.outputModalities))
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
			config.input().TurnDetection = detection
		}
	}
	if f.inputAudioTranscription != nil {
		transcription, err := d.wireTranscription(*f.inputAudioTranscription)
		if err != nil {
			return nil, err
		}
		config.input().Transcription = transcription
	}
	if f.tools != nil || (f.toolChoice != nil && !d.toolChoiceField) {
		tools := effective.Tools
		if d.withholdsTools(effective.ToolChoice) {
			tools = nil
		}
		var err error
		if config.Tools, err = d.wireTools(tools); err != nil {
			return nil, err
		}
	}
	var err error
	if config.ToolChoice, err = d.wireToolChoice(f.toolChoice); err != nil {
		return nil, err
	}
	return config, nil
}

// input is the session's `audio.input`, created on first use.
func (c *sessionConfig) input() *audioInput {
	if c.Audio == nil {
		c.Audio = &audioConfig{}
	}
	if c.Audio.Input == nil {
		c.Audio.Input = &audioInput{}
	}
	return c.Audio.Input
}

// wireTranscription is input transcription on the provider's wire, with the
// transcription model the dialect declares for the deployment (the contract:
// "the deployment's own, declared where the deployment is, never chosen by a
// string here"). The client's language is sent in the provider's spelling of
// it (providerconfig.NormalizeLanguage), or not at all when the provider takes
// nothing of it, so the model detects it; a prompt, which the contract defines
// as a hint, is sent where the provider takes one.
func (d *dialect) wireTranscription(value contract.RealtimeInputTranscription) (*transcriptionConfig, error) {
	t := d.transcription
	if t == nil {
		return nil, refused("config.inputAudioTranscription", "this model does not transcribe the caller's speech inside a conversation")
	}
	statement := d.statement()
	wire := &transcriptionConfig{Model: t.model}
	if statement.Parameters.InputTranscriptionPrompt {
		wire.Prompt = value.Prompt
	}
	if value.Language != nil {
		if language, ok := statement.LanguageFor(*value.Language); ok {
			if t.languageHint {
				wire.LanguageHint = &language
			} else {
				wire.Language = &language
			}
		}
	}
	return wire, nil
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
		maxOutputTokens: config.MaxOutputTokens,
	}.wire(d, config)
	if err != nil {
		return nil, err
	}
	input, err := wireFormat(config.InputAudioFormat)
	if err != nil {
		return nil, refused("config.inputAudioFormat", err.Error())
	}
	session.input().Format = input
	voice := config.Voice
	if d.flatSession {
		session.Voice, voice = config.Voice, nil
	}
	output := d.wireOutputFormat(config)
	if output != nil || voice != nil {
		session.Audio.Output = &audioOutput{Voice: voice}
		if output != nil {
			if session.Audio.Output.Format, err = wireFormat(*output); err != nil {
				return nil, refused("config.outputAudioFormat", err.Error())
			}
		}
	}
	return session, nil
}

// wireOutputFormat is the encoding the provider speaks in: the session's, or
// PCM16 for a session that asked for no audio on a provider that answers
// aloud anyway (WireOutputModalities). Kaana then measures that audio and
// relays none of it.
func (d *dialect) wireOutputFormat(config contract.RealtimeSessionConfig) *contract.RealtimeAudioFormat {
	if config.OutputAudioFormat != nil {
		return config.OutputAudioFormat
	}
	if slices.Contains(d.statement().Parameters.WireOutputModalities(config.OutputModalities), contract.RealtimeOutputAudio) {
		pcm := contract.RealtimePCM16
		return &pcm
	}
	return nil
}

// wireUpdate is the session.update a contract session.update command becomes,
// on a session whose configuration, once every update already sent applies,
// is projected.
func (d *dialect) wireUpdate(update contract.RealtimeSessionConfigUpdate, projected contract.RealtimeSessionConfig) (*sessionConfig, error) {
	return sessionFields{
		instructions: update.Instructions, outputModalities: update.OutputModalities, turnDetection: update.TurnDetection,
		inputAudioTranscription: update.InputAudioTranscription, tools: update.Tools, toolChoice: update.ToolChoice,
		maxOutputTokens: update.MaxOutputTokens,
	}.wire(d, mergeUpdate(projected, update))
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
	if update.InputAudioTranscription != nil {
		config.InputAudioTranscription = update.InputAudioTranscription
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
		session, err := s.dialect.wireUpdate(c.Config, s.projectedConfig())
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
			if err := d.refuseUnservable("response", parameters.Fields()); err != nil {
				return event, err
			}
			wire := &responseParameters{Instructions: parameters.Instructions, MaxOutputTokens: d.wireMaxOutputTokens(parameters.MaxOutputTokens)}
			requested := parameters.OutputModalities
			if requested == nil && !d.sessionModalities {
				// The session's modalities, carried on the one field the
				// provider has for them. Every response of a push-to-talk
				// session is created here, so none escapes it.
				requested = s.projectedConfig().OutputModalities
			}
			// What the client asked this response for decides what is relayed
			// of it (relayResponse); the wire is asked for what it can give.
			s.expectResponse(parameters.OutputModalities)
			onWire := wireModalities(d.statement().Parameters.WireOutputModalities(requested))
			if d.responseModalitiesField == "modalities" {
				wire.Modalities = onWire
			} else {
				wire.OutputModalities = onWire
			}
			var err error
			if wire.ToolChoice, err = d.wireToolChoice(parameters.ToolChoice); err != nil {
				return event, err
			}
			event.Response = wire
		} else {
			s.expectResponse(nil)
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

// maxTranscriptionSeconds bounds a reported transcription duration: more than
// a day of audio in one turn of a session that lasts at most an hour is a
// report this adapter cannot have read correctly.
const maxTranscriptionSeconds = 86_400

// transcriptionUnits is what one completed input transcription is billed:
// nothing on a provider whose session price includes it, and on OpenAI the
// ASR model's own usage. A `duration` usage is `input_transcription_milliseconds`,
// read to the microsecond and rounded up to a whole millisecond. A `tokens`
// usage is a charge the declared transcription model is not billed in and the
// contract's unit does not express: the session ends rather than settle a
// guess. A completed event without usage reports none, never zero.
func (d *dialect) transcriptionUnits(event serverEvent) ([]contract.UsageQuantity, error) {
	t := d.transcription
	if t == nil || !t.billed || len(event.Usage) == 0 || string(event.Usage) == "null" {
		return nil, nil
	}
	var usage struct {
		Type    string   `json:"type"`
		Seconds *float64 `json:"seconds"`
	}
	if err := json.Unmarshal(event.Usage, &usage); err != nil {
		return nil, d.invalidEvent(event.Type, "usage"+unreadableKind(err))
	}
	switch usage.Type {
	case "duration":
		if usage.Seconds == nil || math.IsNaN(*usage.Seconds) || *usage.Seconds < 0 || *usage.Seconds > maxTranscriptionSeconds {
			return nil, d.invalidEvent(event.Type, "usage.seconds is not a duration")
		}
		microseconds := int64(math.Round(*usage.Seconds * 1e6))
		milliseconds := int((microseconds + 999) / 1000)
		if milliseconds == 0 {
			return nil, nil
		}
		return []contract.UsageQuantity{{Unit: contract.UnitInputTranscriptionMilliseconds, Quantity: milliseconds}}, nil
	case "tokens":
		return nil, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
			Detail:      fmt.Sprintf("%s billed its input transcription in tokens; %s is declared as billed by duration and a token charge has no unit here", d.name, t.model),
			Passthrough: &contract.ProviderErrorPassthrough{Provider: d.slug}}
	}
	return nil, d.invalidEvent(event.Type, "usage.type is not one this adapter knows")
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
