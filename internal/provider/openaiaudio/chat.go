package openaiaudio

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

// Audio chat: a conversational model answering ALOUD over Chat Completions
// (`gpt-audio-1.5`), requested with the contract's `audioOutput`. It lives here
// rather than in the text chat adapter for the reason the transcription does:
// the `openai-audio` slug is the structural boundary. The publisher attaches an
// audio chat model only to this slug, and this path refuses every chat request
// that does not ask to be answered aloud, so a text chat can never be billed at
// audio rates and an audio answer can never be flattened into text.
//
// Wire reviewed against OpenAI's documentation on 2026-09-30:
//   - https://developers.openai.com/api/docs/guides/audio-chat-completions
//   - https://developers.openai.com/api/reference/resources/chat
//   - https://developers.openai.com/api/docs/models/gpt-audio-1.5

// maxChatResponseBytes bounds a non-streamed answer: the base64 of the largest
// audio the contract can carry, plus its transcript and usage.
const maxChatResponseBytes = 32 << 20

// outputFormats maps the contract's audio output format to OpenAI's `audio.format`
// and to the media type the audio events carry. pcm is OpenAI's `pcm16`: raw
// 16-bit little-endian mono samples with no header, the only format OpenAI
// streams.
var outputFormats = map[contract.AudioOutputFormat]struct{ wire, mediaType string }{
	"wav":                   {"wav", "audio/wav"},
	"mp3":                   {"mp3", "audio/mpeg"},
	"flac":                  {"flac", "audio/flac"},
	"opus":                  {"opus", "audio/ogg"},
	contract.AudioOutputPCM: {"pcm16", "audio/pcm"},
}

// inputAudioFormats is what an `input_audio` part accepts: OpenAI documents wav
// and mp3 only. Anything else is refused rather than relabelled, because a
// wrong format is decoded as noise and billed as audio.
var inputAudioFormats = map[string]string{
	"audio/wav":   "wav",
	"audio/x-wav": "wav",
	"audio/mpeg":  "mp3",
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

// translateChat builds the Chat Completions call. It sends what the caller set
// and nothing else: no voice, format, temperature or token ceiling is chosen
// for a caller who did not choose one.
func (a *Adapter) translateChat(r *contract.Request, route provider.Route) (*provider.Call, error) {
	if r.AudioOutput == nil {
		return refuse("audioOutput", "the OpenAI audio deployment answers aloud only; a text chat belongs to a text deployment")
	}
	if r.Modality != contract.ModalityAudio {
		return refuse("modality", "spoken output requires audio modality")
	}
	format, known := outputFormats[r.AudioOutput.Format]
	if !known {
		return refuse("audioOutput.format", "not an audio output format OpenAI produces")
	}
	if r.Stream && r.AudioOutput.Format != contract.AudioOutputPCM {
		return refuse("audioOutput.format", "OpenAI streams spoken output as pcm only")
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
		translated, err := translateChatMessage(message)
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

	body := chatRequest{
		Model:               route.UpstreamModelID,
		Messages:            messages,
		Modalities:          []string{"text", "audio"},
		Audio:               audioParams{Voice: r.AudioOutput.Voice, Format: format.wire},
		Stream:              r.Stream,
		MaxCompletionTokens: r.MaxOutputTokens,
		Temperature:         r.Sampling.Temperature,
		TopP:                r.Sampling.TopP,
		FrequencyPenalty:    r.Sampling.FrequencyPenalty,
		PresencePenalty:     r.Sampling.PresencePenalty,
		Seed:                r.Sampling.Seed,
		Stop:                r.Sampling.StopSequences,
	}
	if r.Stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaiaudio: encoding the chat request: %w", err)
	}
	call := &provider.Call{Route: route, Method: http.MethodPost, URL: a.base + "/chat/completions", Body: encoded, Header: make(http.Header), Stream: r.Stream, AudioMediaType: format.mediaType}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", map[bool]string{true: "text/event-stream", false: "application/json"}[r.Stream])
	return call, nil
}

// translateChatMessage maps one conversation turn. Text is text; audio is an
// inline wav or mp3 `input_audio` part. Tool turns, images and files have no
// representation on this path, and a remote audio URL would make Kaana the one
// that fetches customer content.
func translateChatMessage(message contract.Message) (chatMessage, error) {
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
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: param + ".mediaType", Detail: "OpenAI accepts wav or mp3 input audio"}
			}
			if len(*source.Data) > base64.StdEncoding.EncodedLen(maxAudioBytes) {
				return chatMessage{}, provider.ErrUnsupported{Code: contract.CodeRequestTooLarge, Param: param, Detail: "input audio exceeds 20 MiB"}
			}
			// Validated as it streams through the decoder: the base64 text is
			// what is forwarded, so the decoded bytes are never kept.
			decoded, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(*source.Data)))
			if err != nil || decoded == 0 || decoded > maxAudioBytes {
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

type chatUsage struct {
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

type upstreamError struct {
	Type    string  `json:"type"`
	Code    *string `json:"code"`
	Message string  `json:"message"`
}

type chatAudio struct {
	Data       *string `json:"data"`
	Transcript *string `json:"transcript"`
}

// chatChoiceBody is what one choice carries, whether as a stream `delta` or a
// whole `message`.
type chatChoiceBody struct {
	Content *string    `json:"content"`
	Refusal *string    `json:"refusal"`
	Audio   *chatAudio `json:"audio"`
}

type chatChunk struct {
	Choices []struct {
		Index        int            `json:"index"`
		Delta        chatChoiceBody `json:"delta"`
		FinishReason *string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage     `json:"usage"`
	Error *upstreamError `json:"error"`
}

type chatCompletion struct {
	Choices []struct {
		Index        int            `json:"index"`
		Message      chatChoiceBody `json:"message"`
		FinishReason *string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func value(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// units applies the contract's audio-token partition (contract set 3.2.0) to
// OpenAI's nested counts. `prompt_tokens` INCLUDES its cached and audio tokens
// and `completion_tokens` its reasoning and audio tokens, so every sibling is
// subtracted out of its parent:
//
//	cached_audio_input_tokens = prompt_tokens_details.cached_tokens_details.audio_tokens
//	audio_input_tokens        = prompt_tokens_details.audio_tokens - cached_audio_input_tokens
//	cached_input_tokens       = prompt_tokens_details.cached_tokens - cached_audio_input_tokens
//	input_tokens              = prompt_tokens - cached_tokens - audio_input_tokens
//	audio_output_tokens       = completion_tokens_details.audio_tokens
//	output_tokens             = completion_tokens - reasoning_tokens - audio_output_tokens
//
// Chat Completions documents no `cached_tokens_details`; when it is absent no
// cached token is attributed to audio. A report whose partition would go
// negative is internally inconsistent, and is refused rather than clamped:
// clamping would bill more units than the provider counted. Audio is reported
// as tokens only, never also as milliseconds.
func (u *chatUsage) units() ([]contract.UsageQuantity, bool) {
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

func invalidChatResponse(detail string) error {
	return provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "OpenAI returned " + detail,
		Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
}

// audioWriter decodes base64 audio and emits it in bounded chunks, counting the
// whole answer against the contract's folded-output ceiling.
type audioWriter struct {
	out       provider.AudioEmitter
	mediaType string
	total     int
}

func (w *audioWriter) write(ctx context.Context, index int, encoded string) error {
	if encoded == "" {
		return nil
	}
	// maxAudioBytes is also the most one folded audio answer may hold at the
	// edge and in the SDK, so an answer longer than that is cut here.
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxAudioBytes-w.total+3 {
		return invalidChatResponse("more audio than one answer may carry")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return invalidChatResponse("audio that is not base64")
	}
	w.total += len(data)
	if w.total > maxAudioBytes {
		return invalidChatResponse("more audio than one answer may carry")
	}
	return provider.EmitAudio(ctx, w.out, index, w.mediaType, data)
}

func (a *Adapter) streamChat(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	audio, ok := out.(provider.AudioEmitter)
	if !ok {
		return outcome, fmt.Errorf("openaiaudio: the emitter cannot carry audio")
	}
	if call.AudioMediaType == "" {
		return outcome, fmt.Errorf("openaiaudio: the translated chat call names no audio format")
	}
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	outcome.KeyID, outcome.KeyClass = key.ID, key.Class
	if err != nil {
		return outcome, err
	}
	defer func() { _ = response.Body.Close() }()
	writer := &audioWriter{out: audio, mediaType: call.AudioMediaType}
	var measured provider.Outcome
	if call.Stream {
		measured, err = a.readChatStream(ctx, response.Body, call, writer, key)
	} else {
		measured, err = a.readChatCompletion(ctx, response.Body, call, writer)
	}
	measured.KeyID, measured.KeyClass = key.ID, key.Class
	return measured, err
}

// readChatStream consumes OpenAI's SSE stream: audio arrives as base64 pcm16 in
// `delta.audio.data`, its words in `delta.audio.transcript`, and usage in the
// last chunk before `[DONE]`.
func (a *Adapter) readChatStream(ctx context.Context, body io.Reader, call *provider.Call, audio *audioWriter, key provider.Key) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	out := audio.out
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	decoder := sse.NewDecoder(body)
	terminated := false
	for !terminated {
		frame, more := decoder.Next()
		if !more {
			break
		}
		if frame.Data == "" {
			continue
		}
		if frame.Data == "[DONE]" {
			terminated = true
			continue
		}
		var chunk chatChunk
		if json.Unmarshal([]byte(frame.Data), &chunk) != nil {
			return outcome, invalidChatResponse("a stream frame this adapter cannot read")
		}
		if chunk.Error != nil {
			return outcome, a.streamFailure(*chunk.Error, key)
		}
		if err := applyUsage(chunk.Usage, &outcome); err != nil {
			return outcome, err
		}
		for _, choice := range chunk.Choices {
			if err := emitChoice(ctx, audio, choice.Index, choice.Delta, choice.FinishReason, &outcome); err != nil {
				return outcome, err
			}
		}
	}
	if err := decoder.Err(); err != nil {
		return outcome, a.TransportFailure(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if !terminated {
		return outcome, a.TransportFailure(ctx, io.ErrUnexpectedEOF)
	}
	return outcome, finishAnswer(out, &outcome)
}

// readChatCompletion consumes a non-streamed answer: the whole audio file in
// `message.audio.data` and its words in `message.audio.transcript`. The same
// normalized events come out as from a stream, the audio in bounded chunks.
func (a *Adapter) readChatCompletion(ctx context.Context, body io.Reader, call *provider.Call, audio *audioWriter) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	raw, err := io.ReadAll(io.LimitReader(body, maxChatResponseBytes+1))
	if err != nil {
		return outcome, a.TransportFailure(ctx, err)
	}
	if len(raw) > maxChatResponseBytes {
		return outcome, invalidChatResponse("an oversized chat response")
	}
	var completion chatCompletion
	if json.Unmarshal(raw, &completion) != nil {
		return outcome, invalidChatResponse("a chat response this adapter cannot read")
	}
	// Units are measured before anything is delivered: OpenAI bills the
	// answer it generated whether or not it reaches the customer.
	if err := applyUsage(completion.Usage, &outcome); err != nil {
		return outcome, err
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	out := audio.out
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return outcome, err
	}
	for _, choice := range completion.Choices {
		if err := emitChoice(ctx, audio, choice.Index, choice.Message, choice.FinishReason, &outcome); err != nil {
			return outcome, err
		}
	}
	return outcome, finishAnswer(out, &outcome)
}

// applyUsage records a reported usage block on the outcome, refusing one whose
// parts do not add up rather than clamping it.
func applyUsage(usage *chatUsage, outcome *provider.Outcome) error {
	if usage == nil {
		return nil
	}
	units, consistent := usage.units()
	if !consistent {
		return invalidChatResponse("an inconsistent usage report")
	}
	outcome.Units, outcome.UsageSource = units, contract.UsageProviderReported
	return nil
}

// emitChoice forwards one choice's text, spoken words and audio, and records
// its finish reason.
func emitChoice(ctx context.Context, audio *audioWriter, index int, body chatChoiceBody, finish *string, outcome *provider.Outcome) error {
	if err := emitText(audio.out, index, body.Content, body.Refusal); err != nil {
		return err
	}
	if spoken := body.Audio; spoken != nil {
		if spoken.Transcript != nil && *spoken.Transcript != "" {
			if err := audio.out.Delta(index, contract.ChannelOutputAudioTranscript, *spoken.Transcript); err != nil {
				return err
			}
		}
		if spoken.Data != nil {
			if err := audio.write(ctx, index, *spoken.Data); err != nil {
				return err
			}
		}
	}
	if finish != nil {
		outcome.FinishReason = finishReason(*finish)
	}
	return nil
}

// finishAnswer emits reported usage and settles the finish reason an answer
// that named none ended with.
func finishAnswer(out provider.Emitter, outcome *provider.Outcome) error {
	if outcome.UsageSource == contract.UsageProviderReported {
		if err := out.Usage(outcome.Units, outcome.UsageSource); err != nil {
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

// streamFailure classifies an error object that arrived inside the stream,
// after a 200. There is no status, so OpenAI's own error type and code are all
// there is; an unrecognised one is an unattributed provider failure.
func (a *Adapter) streamFailure(reported upstreamError, key provider.Key) error {
	failure := provider.ErrUpstream{Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
	if reported.Type != "" {
		kind := contract.SafeErrorText(provider.RedactSecret(reported.Type, key.Secret()))
		failure.Passthrough.Code = &kind
	}
	if reported.Message != "" {
		message := contract.SafeErrorText(provider.RedactSecret(reported.Message, key.Secret()))
		failure.Passthrough.Message = &message
	}
	code := ""
	if reported.Code != nil {
		code = *reported.Code
	}
	switch {
	case reported.Type == "insufficient_quota" || code == "insufficient_quota":
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own OpenAI account cannot be billed for this request"
	case code == "invalid_api_key" || reported.Type == "authentication_error":
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = "OpenAI refused the platform's credential for this route"
	case code == "rate_limit_exceeded" || reported.Type == "rate_limit_error":
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = "OpenAI rate-limited this request part-way through it"
	case reported.Type == "server_error":
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "OpenAI failed part-way through the response"
	default:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamUnknown
		failure.Detail = "OpenAI failed part-way through the response and named no reason this build knows"
	}
	return provider.CustomerCredentialFailure(key, failure)
}

// isChatCall reports whether a translated call is the audio chat one.
func isChatCall(call *provider.Call) bool {
	return call != nil && strings.HasSuffix(call.URL, "/chat/completions")
}
