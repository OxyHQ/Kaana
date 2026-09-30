// Package spokenchat is Chat Completions answered ALOUD: a conversational
// model asked with the contract's `audioOutput` to reply in its own voice, the
// audio streaming as audio events and its words on the output_audio_transcript
// channel (contract set 3.2.0).
//
// It is one wire, spoken by more than one provider: OpenAI's own origin (the
// `openai-audio` slug) and OpenRouter's gateway rows for OpenAI's audio models
// (`openai/gpt-audio*` under `openrouter`). The request mapping, the reader and
// the audio-token partition are the same on both, so they live here once and
// each adapter supplies only what is genuinely its own: the endpoint, whether
// the provider streams spoken output only, any provider-policy object the body
// must carry, and how its in-stream error vocabulary is classified. Which
// deployments may speak at all is providerconfig.SpeaksAloud — decided per slug
// and model, not per protocol — and the executor refuses a spoken request to a
// deployment that cannot before Translate runs.
//
// Wire reviewed on 2026-09-30:
//   - OpenAI: https://developers.openai.com/api/docs/guides/audio-chat-completions,
//     https://developers.openai.com/api/reference/resources/chat,
//     https://developers.openai.com/api/docs/models/gpt-audio-1.5
//   - OpenRouter: https://openrouter.ai/docs/guides/overview/multimodal/audio,
//     https://openrouter.ai/docs/api/api-reference/chat/create-a-chat-completion,
//     https://openrouter.ai/docs/cookbook/administration/usage-accounting
package spokenchat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/sse"
)

// MaxAudioBytes is the contract's inline audio ceiling (MAX_INFERENCE_AUDIO_BYTES,
// 20 MiB). It bounds one input audio part and one whole spoken answer, the most
// the edge can fold into one response.
const MaxAudioBytes = 20 << 20

// maxResponseBytes bounds a non-streamed answer: the base64 of the largest
// audio the contract can carry, plus its transcript and usage.
const maxResponseBytes = 32 << 20

// outputFormats maps the contract's audio output format to the wire's
// `audio.format` and to the media type the audio events carry. pcm is the
// wire's `pcm16`: raw 16-bit little-endian mono samples with no header.
var outputFormats = map[contract.AudioOutputFormat]struct{ wire, mediaType string }{
	"wav":                   {"wav", "audio/wav"},
	"mp3":                   {"mp3", "audio/mpeg"},
	"flac":                  {"flac", "audio/flac"},
	"opus":                  {"opus", "audio/ogg"},
	contract.AudioOutputPCM: {"pcm16", "audio/pcm"},
}

// inputAudioFormats is what an `input_audio` part accepts: OpenAI documents wav
// and mp3 for these models, and OpenRouter forwards the part to the same
// upstream ("Supported formats vary by provider"). Anything else is refused
// rather than relabelled, because a wrong format is decoded as noise and
// billed as audio.
var inputAudioFormats = map[string]string{
	"audio/wav":   "wav",
	"audio/x-wav": "wav",
	"audio/mpeg":  "mp3",
}

// Dialect is what differs between the providers that speak this wire.
type Dialect struct {
	// Provider is the slug every failure is attributed to.
	Provider contract.ProviderSlug
	// Name is how a failure detail names the provider.
	Name string
	// StreamOnly is a provider that delivers spoken output only as a stream
	// (OpenRouter: "Audio output requires streaming (`stream: true`)"). The
	// upstream is then always streamed; a customer who asked for the whole
	// answer receives the same normalized events, which the edge folds.
	StreamOnly bool
}

// Failures classifies what only the adapter can: a transport failure, and an
// error object that arrived inside a 200 stream, in the provider's own
// vocabulary. Refused responses go through the adapter's provider.Walk sender.
type Failures interface {
	TransportFailure(ctx context.Context, err error) error
	StreamFailure(reported StreamError, key provider.Key) error
}

// StreamError is an `error` object inside a stream frame.
type StreamError struct {
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Message string `json:"message"`
}

// CodeString is the error's code when it is a string, and "" otherwise
// (OpenRouter documents numeric codes).
func (e StreamError) CodeString() string {
	code, _ := e.Code.(string)
	return code
}

type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	Modalities          []string       `json:"modalities"`
	Audio               audioParams    `json:"audio"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	TopP                *float64       `json:"top_p,omitempty"`
	FrequencyPenalty    *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64       `json:"presence_penalty,omitempty"`
	Seed                *int           `json:"seed,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	// Provider is a provider's own routing-policy object (OpenRouter's), set
	// by the adapter and never by a caller.
	Provider any `json:"provider,omitempty"`
}

type audioParams struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role    string  `json:"role"`
	Content any     `json:"content"`
	Name    *string `json:"name,omitempty"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type audioPart struct {
	Type       string `json:"type"`
	InputAudio struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
}

func refuse(param, message string) (*provider.Call, error) {
	return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: message}
}

// Translate builds the Chat Completions call to endpoint. It sends what the
// caller set and nothing else: no voice, format, temperature or token ceiling
// is chosen for a caller who did not choose one. policy, when non-nil, is the
// provider's own routing-policy object.
func Translate(r *contract.Request, route provider.Route, endpoint string, dialect Dialect, policy any) (*provider.Call, error) {
	if r.AudioOutput == nil {
		return refuse("audioOutput", "this path answers aloud only; a text chat belongs to the text path")
	}
	if r.Modality != contract.ModalityAudio {
		return refuse("modality", "spoken output requires audio modality")
	}
	format, known := outputFormats[r.AudioOutput.Format]
	if !known {
		return refuse("audioOutput.format", "not an audio output format the audio chat models produce")
	}
	if r.Stream && r.AudioOutput.Format != contract.AudioOutputPCM {
		return refuse("audioOutput.format", "spoken output streams as pcm only")
	}
	if r.AudioOutput.Voice == "" {
		return refuse("audioOutput.voice", "a voice is required")
	}
	switch {
	case r.Speech != nil:
		return refuse("speech", "speech parameters belong to audio_speech")
	case r.Reasoning != nil:
		return refuse("reasoning.effort", "the audio chat models take no reasoning effort")
	case len(r.Tools) != 0 || r.ToolChoice != nil:
		return refuse("tools", "tool calling is not implemented for spoken output")
	case r.ResponseFormat != nil && r.ResponseFormat.Type != contract.ResponseFormatText:
		return refuse("responseFormat", "structured output cannot be spoken")
	case r.Sampling.TopK != nil:
		return refuse("sampling.topK", "chat completions has no top_k parameter")
	}
	if r.Input.Format != contract.InputMessages || len(r.Input.Messages) == 0 {
		return refuse("input.format", "spoken output requires a messages input")
	}
	messages := make([]chatMessage, 0, len(r.Input.Messages))
	for index, message := range r.Input.Messages {
		translated, err := translateMessage(message)
		if err != nil {
			var unsupported provider.ErrUnsupported
			if errors.As(err, &unsupported) {
				unsupported.Param = fmt.Sprintf("input.messages[%d].%s", index, unsupported.Param)
				return nil, unsupported
			}
			return nil, err
		}
		messages = append(messages, translated)
	}

	stream := r.Stream || dialect.StreamOnly
	body := chatRequest{
		Model:               route.UpstreamModelID,
		Messages:            messages,
		Modalities:          []string{"text", "audio"},
		Audio:               audioParams{Voice: r.AudioOutput.Voice, Format: format.wire},
		Stream:              stream,
		MaxCompletionTokens: r.MaxOutputTokens,
		Temperature:         r.Sampling.Temperature,
		TopP:                r.Sampling.TopP,
		FrequencyPenalty:    r.Sampling.FrequencyPenalty,
		PresencePenalty:     r.Sampling.PresencePenalty,
		Seed:                r.Sampling.Seed,
		Stop:                r.Sampling.StopSequences,
		Provider:            policy,
	}
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("spokenchat: encoding the chat request: %w", err)
	}
	// Stream is the UPSTREAM framing here: the customer's choice, unless the
	// provider only streams spoken output.
	call := &provider.Call{Route: route, Method: http.MethodPost, URL: endpoint, Body: encoded, Header: make(http.Header), Stream: stream, AudioMediaType: format.mediaType}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", map[bool]string{true: "text/event-stream", false: "application/json"}[stream])
	return call, nil
}

// IsCall reports whether a translated call is a spoken chat call: only this
// package's Translate names an audio format on a Chat Completions call.
func IsCall(call *provider.Call) bool {
	return call != nil && call.AudioMediaType != "" && strings.HasSuffix(call.URL, "/chat/completions")
}

// translateMessage maps one conversation turn. Text is text; audio is an
// inline wav or mp3 `input_audio` part. Tool turns, images and files have no
// representation on this path, and a remote audio URL would make Kaana the one
// that fetches customer content.
func translateMessage(message contract.Message) (chatMessage, error) {
	switch message.Role {
	case contract.RoleSystem, contract.RoleDeveloper, contract.RoleUser, contract.RoleAssistant:
	default:
		return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "role", Detail: "tool turns are not implemented for spoken output"}
	}
	if len(message.ToolCalls) != 0 || message.ToolCallID != nil {
		return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "toolCalls", Detail: "tool turns are not implemented for spoken output"}
	}
	translated := chatMessage{Role: string(message.Role), Name: message.Name}
	if len(message.Content) == 1 && message.Content[0].Type == contract.ContentPartText && message.Content[0].Text != nil {
		translated.Content = *message.Content[0].Text
		return translated, nil
	}
	parts := make([]any, 0, len(message.Content))
	for index, part := range message.Content {
		param := fmt.Sprintf("content[%d]", index)
		switch part.Type {
		case contract.ContentPartText:
			if part.Text == nil {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: "a text part carries no text"}
			}
			parts = append(parts, textPart{Type: "text", Text: *part.Text})
		case contract.ContentPartAudio:
			if message.Role != contract.RoleUser {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: param, Detail: "only a user turn carries input audio"}
			}
			source := part.Source
			if source == nil || source.Kind != contract.ContentSourceInline || source.URL != nil || source.Data == nil || source.MediaType == nil {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: param, Detail: "audio is taken inline; a url source would require Kaana to fetch customer content"}
			}
			wire, accepted := inputAudioFormats[strings.ToLower(*source.MediaType)]
			if !accepted {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: param + ".mediaType", Detail: "the audio chat models accept wav or mp3 input audio"}
			}
			if len(*source.Data) > base64.StdEncoding.EncodedLen(MaxAudioBytes) {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeRequestTooLarge, Param: param, Detail: "input audio exceeds 20 MiB"}
			}
			// Validated as it streams through the decoder: the base64 text is
			// what is forwarded, so the decoded bytes are never kept.
			decoded, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(*source.Data)))
			if err != nil || decoded == 0 || decoded > MaxAudioBytes {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: "input audio must be nonempty base64 of at most 20 MiB"}
			}
			audio := audioPart{Type: "input_audio"}
			audio.InputAudio.Data, audio.InputAudio.Format = *source.Data, wire
			parts = append(parts, audio)
		default:
			return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: param, Detail: fmt.Sprintf("the audio chat models take text and audio; a %q part has no representation", part.Type)}
		}
	}
	if len(parts) == 0 {
		return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "content", Detail: "a turn carries no content"}
	}
	translated.Content = parts
	return translated, nil
}

/* -------------------------------------------------------------------------- */
/*  Responses                                                                 */
/* -------------------------------------------------------------------------- */

type usage struct {
	PromptTokens        *int `json:"prompt_tokens"`
	CompletionTokens    *int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens        *int `json:"cached_tokens"`
		AudioTokens         *int `json:"audio_tokens"`
		CachedTokensDetails *struct {
			AudioTokens *int `json:"audio_tokens"`
		} `json:"cached_tokens_details"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
		AudioTokens     *int `json:"audio_tokens"`
	} `json:"completion_tokens_details"`
}

type spokenAudio struct {
	Data       *string `json:"data"`
	Transcript *string `json:"transcript"`
}

// choiceBody is what one choice carries, whether as a stream `delta` or a
// whole `message`.
type choiceBody struct {
	Content *string      `json:"content"`
	Refusal *string      `json:"refusal"`
	Audio   *spokenAudio `json:"audio"`
}

type chunk struct {
	Choices []struct {
		Index        int        `json:"index"`
		Delta        choiceBody `json:"delta"`
		FinishReason *string    `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage       `json:"usage"`
	Error *StreamError `json:"error"`
}

type completion struct {
	Choices []struct {
		Index        int        `json:"index"`
		Message      choiceBody `json:"message"`
		FinishReason *string    `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

func value(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// units applies the contract's audio-token partition (contract set 3.2.0) to
// the wire's nested counts. `prompt_tokens` INCLUDES its cached and audio
// tokens and `completion_tokens` its reasoning and audio tokens, so every
// sibling is subtracted out of its parent:
//
//	cached_audio_input_tokens = prompt_tokens_details.cached_tokens_details.audio_tokens
//	audio_input_tokens        = prompt_tokens_details.audio_tokens - cached_audio_input_tokens
//	cached_input_tokens       = prompt_tokens_details.cached_tokens - cached_audio_input_tokens
//	input_tokens              = prompt_tokens - cached_tokens - audio_input_tokens
//	audio_output_tokens       = completion_tokens_details.audio_tokens
//	output_tokens             = completion_tokens - reasoning_tokens - audio_output_tokens
//
// Chat Completions documents no `cached_tokens_details`; when it is absent no
// cached token is attributed to audio. OpenRouter's usage object carries the
// same fields with the same nesting ("audio_tokens": "Audio input tokens",
// "Tokens used for audio output"). A report whose partition would go negative
// is internally inconsistent, and is refused rather than clamped: clamping
// would bill more units than the provider counted. Audio is reported as
// tokens only, never also as milliseconds.
func (u *usage) units() ([]contract.UsageQuantity, bool) {
	if u == nil || u.PromptTokens == nil || u.CompletionTokens == nil {
		return nil, false
	}
	var cached, audioIn, cachedAudio, reasoning, audioOut int
	if d := u.PromptTokensDetails; d != nil {
		cached, audioIn = value(d.CachedTokens), value(d.AudioTokens)
		if d.CachedTokensDetails != nil {
			cachedAudio = value(d.CachedTokensDetails.AudioTokens)
		}
	}
	if d := u.CompletionTokensDetails; d != nil {
		reasoning, audioOut = value(d.ReasoningTokens), value(d.AudioTokens)
	}
	audioIn -= cachedAudio
	cachedText := cached - cachedAudio
	input := *u.PromptTokens - cached - audioIn
	output := *u.CompletionTokens - reasoning - audioOut
	for _, quantity := range []int{cached, cachedAudio, cachedText, audioIn, input, reasoning, audioOut, output} {
		if quantity < 0 {
			return nil, false
		}
	}
	units := []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1},
		{Unit: contract.UnitInputTokens, Quantity: input},
	}
	for _, sibling := range []contract.UsageQuantity{
		{Unit: contract.UnitCachedInputTokens, Quantity: cachedText},
		{Unit: contract.UnitAudioInputTokens, Quantity: audioIn},
		{Unit: contract.UnitCachedAudioInputTokens, Quantity: cachedAudio},
	} {
		if sibling.Quantity > 0 {
			units = append(units, sibling)
		}
	}
	units = append(units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: output})
	for _, sibling := range []contract.UsageQuantity{
		{Unit: contract.UnitReasoningTokens, Quantity: reasoning},
		{Unit: contract.UnitAudioOutputTokens, Quantity: audioOut},
	} {
		if sibling.Quantity > 0 {
			units = append(units, sibling)
		}
	}
	return units, true
}

// reader is one spoken answer being read.
type reader struct {
	dialect   Dialect
	failures  Failures
	key       provider.Key
	out       provider.AudioEmitter
	mediaType string
	total     int
}

func (r *reader) invalid(detail string) error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: r.dialect.Name + " returned " + detail,
		Passthrough: &contract.ProviderErrorPassthrough{Provider: r.dialect.Provider}}
}

// Read consumes the response to a spoken call — a stream or a whole answer,
// as the call asked for — and emits its text, transcript and audio. The
// outcome carries the units measured so far even when it fails, so a partial
// answer settles on what the provider already billed.
func Read(ctx context.Context, body io.Reader, call *provider.Call, out provider.Emitter, key provider.Key, dialect Dialect, failures Failures) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	audio, ok := out.(provider.AudioEmitter)
	if !ok {
		return outcome, fmt.Errorf("spokenchat: the emitter cannot carry audio")
	}
	if call.AudioMediaType == "" {
		return outcome, fmt.Errorf("spokenchat: the translated chat call names no audio format")
	}
	answer := &reader{dialect: dialect, failures: failures, key: key, out: audio, mediaType: call.AudioMediaType}
	if call.Stream {
		return answer.readStream(ctx, body, call)
	}
	return answer.readCompletion(ctx, body, call)
}

// write decodes base64 audio and emits it in bounded chunks, counting the
// whole answer against the contract's folded-output ceiling.
func (r *reader) write(ctx context.Context, index int, encoded string) error {
	if encoded == "" {
		return nil
	}
	// MaxAudioBytes is also the most one folded audio answer may hold at the
	// edge and in the SDK, so an answer longer than that is cut here.
	if base64.StdEncoding.DecodedLen(len(encoded)) > MaxAudioBytes-r.total+3 {
		return r.invalid("more audio than one answer may carry")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return r.invalid("audio that is not base64")
	}
	r.total += len(data)
	if r.total > MaxAudioBytes {
		return r.invalid("more audio than one answer may carry")
	}
	return provider.EmitAudio(ctx, r.out, index, r.mediaType, data)
}

// readStream consumes an SSE stream: audio arrives as base64 in
// `delta.audio.data`, its words in `delta.audio.transcript`, and usage in the
// last chunk before `[DONE]`.
func (r *reader) readStream(ctx context.Context, body io.Reader, call *provider.Call) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	if err := r.out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	decoder := sse.NewDecoder(body)
	terminated := false
	for !terminated {
		frame, more := decoder.Next()
		if !more {
			break
		}
		// OpenRouter interleaves SSE comments (": OPENROUTER PROCESSING")
		// while an upstream is busy; they carry no data and are skipped here.
		if frame.Data == "" {
			continue
		}
		if frame.Data == "[DONE]" {
			terminated = true
			continue
		}
		var parsed chunk
		if json.Unmarshal([]byte(frame.Data), &parsed) != nil {
			return outcome, r.invalid("a stream frame this adapter cannot read")
		}
		if parsed.Error != nil {
			return outcome, r.failures.StreamFailure(*parsed.Error, r.key)
		}
		if err := r.applyUsage(parsed.Usage, &outcome); err != nil {
			return outcome, err
		}
		for _, choice := range parsed.Choices {
			if err := r.emitChoice(ctx, choice.Index, choice.Delta, choice.FinishReason, &outcome); err != nil {
				return outcome, err
			}
		}
	}
	if err := decoder.Err(); err != nil {
		return outcome, r.failures.TransportFailure(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if !terminated {
		return outcome, r.failures.TransportFailure(ctx, io.ErrUnexpectedEOF)
	}
	return outcome, r.finish(&outcome)
}

// readCompletion consumes a non-streamed answer: the whole audio file in
// `message.audio.data` and its words in `message.audio.transcript`. The same
// normalized events come out as from a stream, the audio in bounded chunks.
func (r *reader) readCompletion(ctx context.Context, body io.Reader, call *provider.Call) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return outcome, r.failures.TransportFailure(ctx, err)
	}
	if len(raw) > maxResponseBytes {
		return outcome, r.invalid("an oversized chat response")
	}
	var parsed completion
	if json.Unmarshal(raw, &parsed) != nil {
		return outcome, r.invalid("a chat response this adapter cannot read")
	}
	// Units are measured before anything is delivered: the provider bills the
	// answer it generated whether or not it reaches the customer.
	if err := r.applyUsage(parsed.Usage, &outcome); err != nil {
		return outcome, err
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if err := r.out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	for _, choice := range parsed.Choices {
		if err := r.emitChoice(ctx, choice.Index, choice.Message, choice.FinishReason, &outcome); err != nil {
			return outcome, err
		}
	}
	return outcome, r.finish(&outcome)
}

// applyUsage records a reported usage block on the outcome, refusing one whose
// parts do not add up rather than clamping it.
func (r *reader) applyUsage(reported *usage, outcome *provider.Outcome) error {
	if reported == nil {
		return nil
	}
	units, consistent := reported.units()
	if !consistent {
		return r.invalid("an inconsistent usage report")
	}
	outcome.Units, outcome.UsageSource = units, contract.UsageProviderReported
	return nil
}

// emitChoice forwards one choice's text, spoken words and audio, and records
// its finish reason.
func (r *reader) emitChoice(ctx context.Context, index int, body choiceBody, finish *string, outcome *provider.Outcome) error {
	if err := emitText(r.out, index, body.Content, body.Refusal); err != nil {
		return err
	}
	if spoken := body.Audio; spoken != nil {
		if spoken.Transcript != nil && *spoken.Transcript != "" {
			if err := r.out.Delta(index, contract.ChannelOutputAudioTranscript, *spoken.Transcript); err != nil {
				return err
			}
		}
		if spoken.Data != nil {
			if err := r.write(ctx, index, *spoken.Data); err != nil {
				return err
			}
		}
	}
	if finish != nil {
		outcome.FinishReason = finishReason(*finish)
	}
	return nil
}

// finish emits reported usage and settles the finish reason an answer that
// named none ended with.
func (r *reader) finish(outcome *provider.Outcome) error {
	if outcome.UsageSource == contract.UsageProviderReported {
		if err := r.out.Usage(outcome.Units, outcome.UsageSource); err != nil {
			return err
		}
	}
	if outcome.FinishReason == "" {
		outcome.FinishReason = contract.FinishStop
	}
	return nil
}

// emitText forwards text the model wrote rather than spoke, and a refusal, on
// their own channels. A transcript is never among them.
func emitText(out provider.Emitter, index int, content, refusal *string) error {
	if content != nil && *content != "" {
		if err := out.Delta(index, contract.ChannelOutputText, *content); err != nil {
			return err
		}
	}
	if refusal != nil && *refusal != "" {
		return out.Delta(index, contract.ChannelRefusal, *refusal)
	}
	return nil
}

func finishReason(reason string) contract.FinishReason {
	switch reason {
	case "length":
		return contract.FinishLength
	case "content_filter":
		return contract.FinishContentFilter
	default:
		// `stop`, and anything this build does not know, is reported as a
		// normal stop rather than a guessed category.
		return contract.FinishStop
	}
}
