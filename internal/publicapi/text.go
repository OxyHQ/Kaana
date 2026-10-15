package publicapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/sse"
)

// collected is what a text execution produced, folded from its events.
type collected struct {
	started      bool
	model        contract.ModelReference
	generationID *contract.GenerationID
	startedAt    contract.Timestamp
	text         bytes.Buffer
	refusal      bytes.Buffer
	transcript   bytes.Buffer
	audio        bytes.Buffer
	toolOrder    []string
	tools        map[string]*contract.ToolCall
	finish       contract.FinishReason
	completedAt  contract.Timestamp
}

func newCollected() *collected { return &collected{tools: make(map[string]*contract.ToolCall)} }

// fold records one event.
func (c *collected) fold(event contract.StreamEvent) error {
	switch e := event.(type) {
	case *contract.StreamStartEvent:
		c.started, c.model, c.generationID, c.startedAt = true, e.ResolvedModelReference, e.GenerationID, e.StartedAt
	case *contract.StreamDeltaEvent:
		switch e.Channel {
		case contract.ChannelOutputText:
			c.text.WriteString(e.Text)
		case contract.ChannelRefusal:
			c.refusal.WriteString(e.Text)
		case contract.ChannelOutputAudioTranscript:
			c.transcript.WriteString(e.Text)
		}
	case *contract.StreamAudioEvent:
		decoded, err := base64.StdEncoding.DecodeString(e.Data)
		if err != nil {
			return err
		}
		if c.audio.Len()+len(decoded) > maxSpeechAudioBytes {
			return errAnswerTooLarge
		}
		c.audio.Write(decoded)
	case *contract.StreamToolCallEvent:
		call, known := c.tools[e.ToolCallID]
		if !known {
			call = &contract.ToolCall{ID: e.ToolCallID}
			c.tools[e.ToolCallID] = call
			c.toolOrder = append(c.toolOrder, e.ToolCallID)
		}
		if e.Name != nil {
			call.Name = *e.Name
		}
		if e.ArgumentsDelta != nil {
			call.Arguments += *e.ArgumentsDelta
		}
	case *contract.StreamDoneEvent:
		c.finish, c.completedAt = e.FinishReason, e.CompletedAt
		if e.GenerationID != nil {
			c.generationID = e.GenerationID
		}
	}
	return nil
}

// message is the assistant message the events fold into.
func (c *collected) message() contract.Message {
	message := contract.Message{Role: contract.RoleAssistant, Content: []contract.ContentPart{}}
	text := c.text.String() + c.transcript.String()
	if text != "" {
		message.Content = append(message.Content, contract.ContentPart{Type: contract.ContentPartText, Text: &text})
	}
	if c.refusal.Len() > 0 {
		refusal := c.refusal.String()
		message.Content = append(message.Content, contract.ContentPart{Type: contract.ContentPartRefusal, Text: &refusal})
	}
	for _, id := range c.toolOrder {
		message.ToolCalls = append(message.ToolCalls, *c.tools[id])
	}
	return message
}

// estimatedTokens is a content-free size for Oxy's hold: four characters a
// token, plus a small constant per message, never the text itself.
func estimatedTokens(messages []contract.Message) int {
	total := 0
	for _, message := range messages {
		total += 4
		for _, part := range message.Content {
			if part.Text != nil {
				total += utf16Length(*part.Text)/4 + 1
			}
			if part.Source != nil && part.Source.Data != nil {
				total += len(*part.Source.Data) / 4 / 4
			}
		}
		for _, call := range message.ToolCalls {
			total += len(call.Arguments)/4 + 1
		}
	}
	return total
}

// streamSink translates the executor's events onto a public SSE stream. The
// stream opens on the first event that is not a refusal, so a request refused
// before it started is an HTTP error and not a 200 that fails.
type streamSink struct {
	w         http.ResponseWriter
	requestID contract.RequestID
	writer    *sse.Writer
	sequence  int
	headers   func(http.Header)
	collected *collected
}

func (s *streamSink) open() error {
	if s.writer != nil {
		return nil
	}
	if s.headers != nil {
		s.headers(s.w.Header())
	}
	writer, err := openSSE(s.w, s.requestID)
	if err != nil {
		return err
	}
	s.writer = writer
	return nil
}

func (s *streamSink) next() int {
	sequence := s.sequence
	s.sequence++
	return sequence
}

// sink is the kaana.Sink of a public response stream: provider and
// deployment never reach it (route switches are dropped, the usage event loses
// its deployment, errors are projected), and the sequence is renumbered so it
// stays contiguous.
func (s *streamSink) sink(event contract.StreamEvent) error {
	if err := s.collected.fold(event); err != nil {
		return err
	}
	var public any
	switch e := event.(type) {
	case *contract.StreamStartEvent:
		public = &contract.ResponseStreamStartEvent{SchemaVersion: contract.SchemaVersion, Type: contract.EventStart, RequestID: s.requestID,
			GenerationID: e.GenerationID, Model: e.ResolvedModelReference, StartedAt: e.StartedAt}
	case *contract.StreamDeltaEvent:
		copied := *e
		public = &copied
	case *contract.StreamAudioEvent:
		copied := *e
		public = &copied
	case *contract.StreamToolCallEvent:
		copied := *e
		public = &copied
	case *contract.StreamUsageEvent:
		public = &contract.ResponseStreamUsageEvent{SchemaVersion: contract.SchemaVersion, Type: contract.EventUsage, RequestID: s.requestID,
			Units: e.Units, UsageSource: e.UsageSource}
	case *contract.StreamDoneEvent:
		copied := *e
		copied.ReceiptID = nil
		public = &copied
	case *contract.StreamErrorEvent:
		if s.writer == nil {
			// Refused before anything started: the handler answers it as an
			// HTTP error once the executor returns.
			return nil
		}
		public = &contract.ResponseStreamErrorEvent{SchemaVersion: contract.SchemaVersion, Type: contract.EventError, RequestID: s.requestID,
			Error: *contract.PublicErrorFrom(&e.Error)}
	default:
		return nil
	}
	if err := s.open(); err != nil {
		return err
	}
	sequence := s.next()
	switch e := public.(type) {
	case *contract.ResponseStreamStartEvent:
		e.Seq = sequence
	case *contract.StreamDeltaEvent:
		e.Seq, e.RequestID = sequence, s.requestID
	case *contract.StreamAudioEvent:
		e.Seq, e.RequestID = sequence, s.requestID
	case *contract.StreamToolCallEvent:
		e.Seq, e.RequestID = sequence, s.requestID
	case *contract.ResponseStreamUsageEvent:
		e.Seq = sequence
	case *contract.StreamDoneEvent:
		e.Seq, e.RequestID = sequence, s.requestID
	case *contract.ResponseStreamErrorEvent:
		e.Seq = sequence
	}
	return writeFrame(s.writer, string(event.EventType()), public)
}

/* -------------------------------------------------------------------------- */
/*  POST /v1/responses                                                        */
/* -------------------------------------------------------------------------- */

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	var request responsesRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	if failure := validateResponses(requestID, request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	stream := request.Stream != nil && *request.Stream
	estimate := estimatedTokens(request.Input.messages)
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, authorization{
		operation: contract.GrantResponses, target: request.Model.grantTarget(), body: body, endpoint: "/v1/responses",
		labels: request.Labels, clientRequestID: request.ClientRequestID,
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate, MaxOutputTokens: request.MaxOutputTokens,
			DeclaredTools: len(request.Tools), Stream: stream},
	})
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	envelope := admitted.request(contract.ModalityText, contract.APIFormatResponses, contract.Input{Format: contract.InputMessages, Messages: request.Input.messages})
	envelope.Stream = stream
	envelope.MaxOutputTokens = request.MaxOutputTokens
	envelope.Sampling = contract.SamplingParameters{Temperature: request.Temperature, TopP: request.TopP, TopK: request.TopK,
		FrequencyPenalty: request.FrequencyPenalty, PresencePenalty: request.PresencePenalty, Seed: request.Seed, StopSequences: request.StopSequences}
	envelope.Tools, envelope.ToolChoice, envelope.ResponseFormat, envelope.Reasoning = request.Tools, request.ToolChoice, request.ResponseFormat, request.Reasoning
	policyHeaders := func(header http.Header) {
		header.Set(HeaderRoutingPolicyID, admitted.grant.RoutingPolicy.RoutingPolicyID)
		header.Set(HeaderRoutingPolicyVersion, strconv.Itoa(admitted.grant.RoutingPolicy.PolicyVersion))
	}
	folded := newCollected()
	if stream {
		events := &streamSink{w: w, requestID: requestID, headers: policyHeaders, collected: folded}
		result := s.execute(r.Context(), admitted, &envelope, events.sink)
		if events.writer == nil {
			writeError(w, requestID, failureOf(result, requestID))
		}
		return
	}
	result := s.execute(r.Context(), admitted, &envelope, folded.fold)
	if result.Failure != nil || result.Report == nil {
		writeError(w, requestID, failureOf(result, requestID))
		return
	}
	policyHeaders(w.Header())
	writeJSON(w, requestID, http.StatusOK, response{
		SchemaVersion: contract.SchemaVersion, RequestID: requestID, GenerationID: folded.generationID,
		Model: result.Report.ResolvedModelReference, FinishReason: finishOr(folded.finish),
		Output: []contract.Message{folded.message()}, Usage: nonNilUnits(result.Report.Units),
		RoutingPolicy: admitted.grant.RoutingPolicy, LatencyMs: int(max(0, s.now().Sub(receivedAt).Milliseconds())),
	})
}

func validateResponses(requestID contract.RequestID, request responsesRequest) *contract.PublicError {
	switch {
	case request.MaxOutputTokens != nil && *request.MaxOutputTokens < 1:
		return invalid(requestID, "maxOutputTokens", "maxOutputTokens is positive")
	case request.ToolChoice != nil && len(request.Tools) == 0:
		return invalid(requestID, "toolChoice", "a tool choice requires at least one tool definition")
	case len(request.Tools) > 128:
		return invalid(requestID, "tools", "at most 128 tools")
	case len(request.StopSequences) > 8:
		return invalid(requestID, "stopSequences", "at most 8 stop sequences")
	}
	names := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		if _, duplicate := names[tool.Name]; duplicate {
			return invalid(requestID, "tools", "tool names must be unique within one request")
		}
		names[tool.Name] = struct{}{}
	}
	return nil
}

// readJSON reads a bounded JSON body.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, requestID contract.RequestID) ([]byte, *contract.PublicError) {
	if failure := requireJSON(r, requestID); failure != nil {
		return nil, failure
	}
	return readBody(w, r, requestID, MaxJSONBodyBytes)
}

// failureOf is the public error of a result that did not complete.
func failureOf(result kaana.Result, requestID contract.RequestID) *contract.PublicError {
	if result.Failure != nil {
		return contract.PublicErrorFrom(result.Failure)
	}
	return contract.NewPublicError(requestID, "internal_error", contract.PublicMessage("internal_error"))
}

func finishOr(reason contract.FinishReason) contract.FinishReason {
	if reason == "" {
		return contract.FinishStop
	}
	return reason
}

/* -------------------------------------------------------------------------- */
/*  POST /v1/chat/completions                                                 */
/* -------------------------------------------------------------------------- */

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	refuse := func(failure *contract.PublicError) {
		if failure.RetryAfterMs != nil {
			w.Header().Set("Retry-After", strconv.Itoa((*failure.RetryAfterMs+999)/1000))
		}
		writeJSON(w, requestID, failure.Code.HTTPStatus(), chatErrorOf(failure))
	}
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		refuse(failure)
		return
	}
	var request chatRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		refuse(failure)
		return
	}
	messages, failure := chatMessages(requestID, request)
	if failure != nil {
		refuse(failure)
		return
	}
	stream := request.Stream != nil && *request.Stream
	maxTokens := request.MaxTokens
	if request.MaxCompletionTokens != nil {
		maxTokens = request.MaxCompletionTokens
	}
	estimate := estimatedTokens(messages)
	ask := authorization{
		operation: contract.GrantChatCompletions, target: request.Model.grantTarget(), body: body, endpoint: "/v1/chat/completions",
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate, MaxOutputTokens: maxTokens, DeclaredTools: len(request.Tools), Stream: stream},
	}
	if request.User != nil {
		user := contract.UserID(*request.User)
		ask.onBehalfOf = &user
	}
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, ask)
	if failure != nil {
		refuse(failure)
		return
	}
	envelope := admitted.request(contract.ModalityText, contract.APIFormatChatCompletions, contract.Input{Format: contract.InputMessages, Messages: messages})
	envelope.Stream, envelope.MaxOutputTokens = stream, maxTokens
	envelope.Sampling = contract.SamplingParameters{Temperature: request.Temperature, TopP: request.TopP,
		FrequencyPenalty: request.FrequencyPenalty, PresencePenalty: request.PresencePenalty, Seed: request.Seed}
	if request.Stop != nil {
		envelope.Sampling.StopSequences = request.Stop.sequences
	}
	for _, tool := range request.Tools {
		envelope.Tools = append(envelope.Tools, contract.ToolDefinition{Type: "function", Name: tool.Function.Name,
			Description: tool.Function.Description, Parameters: tool.Function.Parameters, Strict: tool.Function.Strict})
	}
	if request.ToolChoice != nil {
		choice := request.ToolChoice.choice
		envelope.ToolChoice = &choice
	}
	if format := request.ResponseFormat; format != nil {
		envelope.ResponseFormat = &contract.ResponseFormat{Type: contract.ResponseFormatType(format.Type)}
		if format.JSONSchema != nil {
			envelope.ResponseFormat.Name, envelope.ResponseFormat.Schema = &format.JSONSchema.Name, format.JSONSchema.Schema
			strict := format.JSONSchema.Strict != nil && *format.JSONSchema.Strict
			envelope.ResponseFormat.Strict = &strict
		}
	}
	if request.ReasoningEffort != nil {
		envelope.Reasoning = &contract.ReasoningParameters{Effort: *request.ReasoningEffort}
	}
	if request.Audio != nil {
		format := contract.AudioOutputFormat(request.Audio.Format)
		if format == "pcm16" {
			format = contract.AudioOutputPCM
		}
		envelope.AudioOutput = &contract.AudioOutputParameters{Voice: request.Audio.Voice, Format: format}
	}
	created := receivedAt.Unix()
	if stream {
		s.chatStream(r.Context(), w, admitted, &envelope, created)
		return
	}
	folded := newCollected()
	result := s.execute(r.Context(), admitted, &envelope, folded.fold)
	if result.Failure != nil || result.Report == nil {
		refuse(failureOf(result, requestID))
		return
	}
	message := chatChoiceMessage{Role: "assistant"}
	if text := folded.text.String() + folded.transcript.String(); text != "" || len(folded.toolOrder) == 0 {
		message.Content = &text
	}
	for _, id := range folded.toolOrder {
		call := folded.tools[id]
		var output chatOutputToolCall
		output.ID, output.Type, output.Function.Name, output.Function.Arguments = call.ID, "function", call.Name, call.Arguments
		message.ToolCalls = append(message.ToolCalls, output)
	}
	completion := chatCompletion{ID: string(requestID), Object: "chat.completion", Created: created, Model: string(result.Report.ResolvedModelReference),
		Choices: []chatChoice{{Index: 0, Message: message, FinishReason: string(finishOr(folded.finish))}}, Usage: chatUsageOf(result.Report.Units)}
	if folded.audio.Len() > 0 {
		writeJSON(w, requestID, http.StatusOK, chatCompletionWithAudio(completion, folded, receivedAt))
		return
	}
	writeJSON(w, requestID, http.StatusOK, completion)
}

// chatCompletionWithAudio adds the spoken answer to the message.
func chatCompletionWithAudio(completion chatCompletion, folded *collected, at time.Time) any {
	type audio struct {
		ID         string `json:"id"`
		Data       string `json:"data"`
		Transcript string `json:"transcript"`
		ExpiresAt  int64  `json:"expires_at"`
	}
	type message struct {
		chatChoiceMessage
		Audio audio `json:"audio"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	choices := make([]choice, 0, len(completion.Choices))
	for _, original := range completion.Choices {
		choices = append(choices, choice{Index: original.Index, FinishReason: original.FinishReason, Message: message{
			chatChoiceMessage: original.Message,
			Audio: audio{ID: "audio_" + completion.ID, Data: base64.StdEncoding.EncodeToString(folded.audio.Bytes()),
				Transcript: folded.transcript.String(), ExpiresAt: at.Add(time.Hour).Unix()},
		}})
	}
	return struct {
		ID      string    `json:"id"`
		Object  string    `json:"object"`
		Created int64     `json:"created"`
		Model   string    `json:"model"`
		Choices []choice  `json:"choices"`
		Usage   chatUsage `json:"usage"`
	}{completion.ID, completion.Object, completion.Created, completion.Model, choices, completion.Usage}
}

// chatMessages translates and checks the OpenAI-shaped messages.
func chatMessages(requestID contract.RequestID, request chatRequest) ([]contract.Message, *contract.PublicError) {
	if len(request.Messages) == 0 {
		return nil, invalid(requestID, "messages", "messages is at least one message")
	}
	if request.MaxTokens != nil && request.MaxCompletionTokens != nil {
		return nil, invalid(requestID, "max_completion_tokens", "send max_tokens or max_completion_tokens, not both")
	}
	modalities := request.Modalities
	if modalities == nil {
		modalities = []string{"text"}
	}
	spoken := false
	for _, modality := range modalities {
		switch modality {
		case "text":
		case "audio":
			spoken = true
		default:
			return nil, invalid(requestID, "modalities", "modalities are text and audio")
		}
	}
	switch {
	case !contains(modalities, "text"):
		return nil, invalid(requestID, "modalities", "modalities must include text; spoken output always carries its transcript")
	case spoken && request.Audio == nil:
		return nil, invalid(requestID, "audio", "modalities including audio require the audio parameter (voice and format)")
	case !spoken && request.Audio != nil:
		return nil, invalid(requestID, "modalities", "the audio parameter requires modalities to include audio")
	case spoken && request.Stream != nil && *request.Stream && request.Audio.Format != "pcm16":
		return nil, invalid(requestID, "audio.format", "streamed audio output is pcm16; the other formats exist only whole")
	case request.ToolChoice != nil && len(request.Tools) == 0:
		return nil, invalid(requestID, "tool_choice", "a tool choice requires at least one tool definition")
	}
	messages := make([]contract.Message, 0, len(request.Messages))
	for index, message := range request.Messages {
		translated := contract.Message{Role: message.Role, Name: message.Name, ToolCallID: message.ToolCallID, Content: []contract.ContentPart{}}
		if message.Content != nil {
			translated.Content = message.Content.parts
		}
		for _, call := range message.ToolCalls {
			if call.Type != "function" {
				return nil, invalid(requestID, fmt.Sprintf("messages[%d].tool_calls", index), "a tool call is a function call")
			}
			translated.ToolCalls = append(translated.ToolCalls, contract.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
		messages = append(messages, translated)
	}
	return messages, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// chatUsageOf renders measured units in OpenAI's usage block.
func chatUsageOf(units []contract.UsageQuantity) chatUsage {
	var usage chatUsage
	var audioIn, audioOut int
	for _, quantity := range units {
		switch quantity.Unit {
		case contract.UnitInputTokens:
			usage.PromptTokens += quantity.Quantity
		case contract.UnitAudioInputTokens:
			usage.PromptTokens += quantity.Quantity
			audioIn += quantity.Quantity
		case contract.UnitCachedInputTokens:
			usage.PromptTokensDetails.CachedTokens += quantity.Quantity
		case contract.UnitOutputTokens:
			usage.CompletionTokens += quantity.Quantity
		case contract.UnitAudioOutputTokens:
			usage.CompletionTokens += quantity.Quantity
			audioOut += quantity.Quantity
		case contract.UnitReasoningTokens:
			usage.CompletionTokensDetails.ReasoningTokens += quantity.Quantity
		}
	}
	if audioIn > 0 {
		usage.PromptTokensDetails.AudioTokens = &audioIn
	}
	if audioOut > 0 {
		usage.CompletionTokensDetails.AudioTokens = &audioOut
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage
}

// chatStream streams OpenAI chunks: `data: <chunk>` frames, a final chunk
// with the finish reason and usage, then `data: [DONE]`. A failure after the
// first chunk is one `data: <error>` frame and the end, with no [DONE].
func (s *Server) chatStream(ctx context.Context, w http.ResponseWriter, admitted *admission, envelope *contract.Request, created int64) {
	requestID := admitted.requestID
	var writer *dataWriter
	folded := newCollected()
	model := ""
	toolIndex := make(map[string]int)
	write := func(payload any) error {
		if writer == nil {
			opened, err := openDataStream(w, requestID)
			if err != nil {
				return err
			}
			writer = opened
		}
		return writer.write(payload)
	}
	chunk := func(delta chatDelta, finish *string, usage *chatUsage) chatChunk {
		return chatChunk{ID: string(requestID), Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}}, Usage: usage}
	}
	result := s.execute(ctx, admitted, envelope, func(event contract.StreamEvent) error {
		if err := folded.fold(event); err != nil {
			return err
		}
		switch e := event.(type) {
		case *contract.StreamStartEvent:
			model = string(e.ResolvedModelReference)
			role := "assistant"
			return write(chunk(chatDelta{Role: &role}, nil, nil))
		case *contract.StreamDeltaEvent:
			text := e.Text
			switch e.Channel {
			case contract.ChannelOutputText:
				return write(chunk(chatDelta{Content: &text}, nil, nil))
			case contract.ChannelRefusal:
				return write(chunk(chatDelta{Refusal: &text}, nil, nil))
			}
		case *contract.StreamToolCallEvent:
			if e.Complete && e.ArgumentsDelta == nil && e.Name == nil {
				return nil
			}
			index, known := toolIndex[e.ToolCallID]
			var call chatDeltaToolCall
			if !known {
				index = len(toolIndex)
				toolIndex[e.ToolCallID] = index
				id, kind := e.ToolCallID, "function"
				call.ID, call.Type = &id, &kind
			}
			call.Index = index
			call.Function.Name, call.Function.Arguments = e.Name, e.ArgumentsDelta
			if call.Function.Arguments == nil && call.Function.Name == nil {
				empty := ""
				call.Function.Arguments = &empty
			}
			return write(chunk(chatDelta{ToolCalls: []chatDeltaToolCall{call}}, nil, nil))
		}
		return nil
	})
	if writer == nil {
		failure := failureOf(result, requestID)
		writeJSON(w, requestID, failure.Code.HTTPStatus(), chatErrorOf(failure))
		return
	}
	if result.Failure != nil || result.Report == nil {
		_ = writer.write(chatErrorOf(failureOf(result, requestID)))
		return
	}
	finish := string(finishOr(folded.finish))
	usage := chatUsageOf(result.Report.Units)
	if err := write(chunk(chatDelta{}, &finish, &usage)); err != nil {
		return
	}
	_ = writer.done()
}

/* -------------------------------------------------------------------------- */
/*  POST /v1/embeddings                                                       */
/* -------------------------------------------------------------------------- */

// embeddingDimension is the one dimension Kaana's embedding adapters request.
const embeddingDimension = 1024

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	var request embeddingsRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	switch {
	case !request.Model.Valid():
		failure = invalid(requestID, "model", "model is a model reference (publisher/model[@revision])")
	case request.EncodingFormat != nil && *request.EncodingFormat != "float":
		failure = invalid(requestID, "encoding_format", "encoding_format is float")
	case request.Dimensions != nil && *request.Dimensions != embeddingDimension:
		failure = invalid(requestID, "dimensions", fmt.Sprintf("embeddings are served at %d dimensions", embeddingDimension))
	case request.User != nil && (*request.User == "" || len(*request.User) > 64):
		failure = invalid(requestID, "user", "user is 1 to 64 characters")
	}
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	inputs := len(request.Input.texts)
	estimate := 0
	for _, text := range request.Input.texts {
		estimate += utf16Length(text)/4 + 1
	}
	target := contract.ModelTarget(request.Model)
	ask := authorization{operation: contract.GrantEmbeddings, target: &target, body: body, endpoint: "/v1/embeddings",
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate, EmbeddingInputs: &inputs}}
	if request.User != nil {
		user := contract.UserID(*request.User)
		ask.onBehalfOf = &user
	}
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, ask)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	input := contract.Input{Format: contract.InputTextBatch, Texts: request.Input.texts}
	envelope := admitted.request(contract.ModalityEmbedding, contract.APIFormatEmbeddings, input)
	result := s.execute(r.Context(), admitted, &envelope, func(contract.StreamEvent) error { return nil })
	if result.Failure != nil || result.Embedding == nil {
		writeError(w, requestID, failureOf(result, requestID))
		return
	}
	answer := embeddingsResponse{Object: "list", Model: result.Embedding.Model, Data: make([]embeddingVector, 0, len(result.Embedding.Data))}
	for _, vector := range result.Embedding.Data {
		answer.Data = append(answer.Data, embeddingVector{Object: "embedding", Index: vector.Index, Embedding: vector.Embedding})
	}
	answer.Usage.PromptTokens, answer.Usage.TotalTokens = result.Embedding.Usage.InputTokens, result.Embedding.Usage.TotalTokens
	writeJSON(w, requestID, http.StatusOK, answer)
}

/* -------------------------------------------------------------------------- */
/*  POST /v1/decisions                                                        */
/* -------------------------------------------------------------------------- */

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	if err := contract.ValidateDecisionJSONUnicode(body); err != nil {
		writeError(w, requestID, invalid(requestID, "", err.Error()))
		return
	}
	var request decisionRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	input := contract.DecisionInput{State: request.State, Instructions: request.Instructions, Questions: request.Questions, Effort: request.Effort}
	if !request.Model.Valid() || !request.Model.Pinned() {
		writeError(w, requestID, invalid(requestID, "model", "decisions require a revision-pinned model"))
		return
	}
	if err := input.Validate(); err != nil {
		writeError(w, requestID, invalid(requestID, "questions", err.Error()))
		return
	}
	target := contract.ModelTarget(request.Model)
	estimate := (len(body) + 3) / 4
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, authorization{
		operation: contract.GrantDecisions, target: &target, body: body, endpoint: "/v1/decisions",
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate},
	})
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	envelope := admitted.request(contract.ModalityText, contract.APIFormatDecisions, contract.Input{Format: contract.InputDecisions, Decisions: &input})
	result := s.execute(r.Context(), admitted, &envelope, func(contract.StreamEvent) error { return nil })
	if result.Failure != nil || result.Decisions == nil {
		failure := failureOf(result, requestID)
		if len(result.UpstreamCost.Attempts) > 0 && failure.Retryable {
			// A decisions body may have been accepted and billed upstream:
			// it is never retried blindly (docs/decisions.md).
			failure = contract.NewPublicError(requestID, "provider_error", contract.PublicMessage("provider_error"))
		}
		writeError(w, requestID, failure)
		return
	}
	writeJSON(w, requestID, http.StatusOK, decisionSuccess{
		SchemaVersion: contract.SchemaVersion, RequestID: requestID, Model: result.Decisions.Model,
		Data: result.Decisions.Data, Usage: nonNilUnits(result.Decisions.Usage.Units), RoutingPolicy: admitted.grant.RoutingPolicy,
	})
}

/* -------------------------------------------------------------------------- */
/*  data-only SSE (chat completions)                                          */
/* -------------------------------------------------------------------------- */

type dataWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func openDataStream(w http.ResponseWriter, requestID contract.RequestID) (*dataWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("publicapi: %T cannot flush", w)
	}
	header := w.Header()
	header.Set(HeaderRequestID, string(requestID))
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-store")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &dataWriter{w: w, flusher: flusher}, nil
}

func (d *dataWriter) write(payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return d.raw(encoded)
}

func (d *dataWriter) done() error { return d.raw([]byte("[DONE]")) }

func (d *dataWriter) raw(data []byte) error {
	frame := make([]byte, 0, len(data)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	if _, err := d.w.Write(frame); err != nil {
		return errClientGone(err)
	}
	d.flusher.Flush()
	return nil
}
