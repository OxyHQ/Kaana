package publicapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/sse"
)

/* -------------------------------------------------------------------------- */
/*  POST /v1/messages (Anthropic-Messages-compatible)                         */
/* -------------------------------------------------------------------------- */

// headerAnthropicRequestID is where an Anthropic SDK reads the request id.
const headerAnthropicRequestID = "Request-Id"

// handleMessages serves the Anthropic Messages dialect on the `responses`
// operation: it authorizes and executes exactly what handleResponses does and
// only translates the request and the answer. No Oxy grant operation and no
// signed shape is specific to it.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	requestID, receivedAt := NewRequestID(), s.now()
	w.Header().Set(headerAnthropicRequestID, string(requestID))
	refuse := func(failure *contract.PublicError) {
		if failure.RetryAfterMs != nil {
			w.Header().Set("Retry-After", strconv.Itoa((*failure.RetryAfterMs+999)/1000))
		}
		writeJSON(w, requestID, failure.Code.HTTPStatus(), messagesErrorOf(failure))
	}
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		refuse(failure)
		return
	}
	if failure := refuseCacheControl(requestID, body); failure != nil {
		refuse(failure)
		return
	}
	var request messagesRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		refuse(failure)
		return
	}
	if failure := validateMessages(requestID, request); failure != nil {
		refuse(failure)
		return
	}
	messages, paths, failure := translateMessages(requestID, request)
	if failure != nil {
		refuse(failure)
		return
	}
	images, failure := messagesImageInput(requestID, messages, paths)
	if failure != nil {
		refuse(failure)
		return
	}
	stream := request.Stream != nil && *request.Stream
	estimate := estimatedTokens(messages) + estimatedToolTokens(request.Tools)
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, authorization{
		operation: contract.GrantResponses, target: request.Model.grantTarget(), body: body, endpoint: "/v1/messages",
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate, MaxOutputTokens: request.MaxTokens,
			InputImages: images, DeclaredTools: len(request.Tools), Stream: stream},
	})
	if failure != nil {
		refuse(failure)
		return
	}
	s.catalogue.announceLifecycle(w.Header(), &request.Model)
	envelope := admitted.request(contract.ModalityText, contract.APIFormatResponses, contract.Input{Format: contract.InputMessages, Messages: messages})
	envelope.Stream, envelope.MaxOutputTokens = stream, request.MaxTokens
	envelope.Sampling = contract.SamplingParameters{Temperature: request.Temperature, TopP: request.TopP, TopK: request.TopK, StopSequences: request.StopSequences}
	for _, tool := range request.Tools {
		envelope.Tools = append(envelope.Tools, contract.ToolDefinition{Type: "function", Name: tool.Name,
			Description: tool.Description, Parameters: tool.InputSchema, Strict: tool.Strict})
	}
	if request.ToolChoice != nil {
		choice := request.ToolChoice.choice
		envelope.ToolChoice = &choice
	}
	if request.OutputConfig != nil && request.OutputConfig.Effort != nil {
		envelope.Reasoning = &contract.ReasoningParameters{Effort: *request.OutputConfig.Effort}
	}
	if stream {
		s.messagesStream(r.Context(), w, admitted, &envelope)
		return
	}
	folded := newCollected()
	result := s.execute(r.Context(), admitted, &envelope, folded.fold)
	if result.Failure != nil || result.Report == nil {
		refuse(failureOf(result, requestID))
		return
	}
	content, failure := messagesContent(requestID, folded)
	if failure != nil {
		refuse(failure)
		return
	}
	writeJSON(w, requestID, http.StatusOK, messagesAnswer{
		ID: messageID(requestID), Type: "message", Role: "assistant", Model: string(result.Report.ResolvedModelReference),
		Content: content, StopReason: messagesStopReason(finishOr(folded.finish)), Usage: messagesUsageOf(result.Report.Units),
	})
}

// messageID is the id of the message: the request id in Anthropic's spelling.
func messageID(requestID contract.RequestID) string {
	return "msg_" + strings.TrimPrefix(string(requestID), "req_")
}

// messagesContent folds an answer into its output blocks: its words, a
// refusal's words, then each tool call.
func messagesContent(requestID contract.RequestID, folded *collected) ([]messagesOutputBlock, *contract.PublicError) {
	content := []messagesOutputBlock{}
	for _, text := range []string{folded.text.String(), folded.refusal.String()} {
		if text != "" {
			content = append(content, messagesOutputBlock{Type: "text", Text: &text})
		}
	}
	for _, id := range folded.toolOrder {
		call := folded.tools[id]
		input := json.RawMessage("{}")
		if strings.TrimSpace(call.Arguments) != "" {
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(call.Arguments), &object); err != nil || object == nil {
				return nil, contract.NewPublicError(requestID, "provider_error", "the model returned tool arguments that are not a JSON object")
			}
			input = json.RawMessage(call.Arguments)
		}
		content = append(content, messagesOutputBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: input})
	}
	return content, nil
}

/* -------------------------------------------------------------------------- */
/*  Request checks and translation                                            */
/* -------------------------------------------------------------------------- */

// refuseCacheControl refuses a `cache_control` marker wherever the dialect
// puts one: the request, `system`, a tool, a content block and a tool result's
// blocks. It is not read inside a tool's input or input_schema, where a key of
// that name is the caller's own data. Kaana has no usage unit for a cache
// write, so honouring the marker would report less than the request cost; it is
// refused by name and path rather than dropped.
func refuseCacheControl(requestID contract.RequestID, body []byte) *contract.PublicError {
	refusal := func(path string) *contract.PublicError {
		return invalid(requestID, path+".cache_control", "cache_control is not supported: Kaana meters no cache writes, so it would report less than the request cost; remove it")
	}
	root, ok := objectOf(body)
	if !ok {
		return nil
	}
	if _, found := root["cache_control"]; found {
		return invalid(requestID, "cache_control", "cache_control is not supported: Kaana meters no cache writes, so it would report less than the request cost; remove it")
	}
	for index, item := range arrayOf(root["system"]) {
		if object, ok := objectOf(item); ok {
			if _, found := object["cache_control"]; found {
				return refusal(fmt.Sprintf("system[%d]", index))
			}
		}
	}
	for index, item := range arrayOf(root["tools"]) {
		if object, ok := objectOf(item); ok {
			if _, found := object["cache_control"]; found {
				return refusal(fmt.Sprintf("tools[%d]", index))
			}
		}
	}
	for messageIndex, item := range arrayOf(root["messages"]) {
		message, ok := objectOf(item)
		if !ok {
			continue
		}
		for blockIndex, raw := range arrayOf(message["content"]) {
			block, ok := objectOf(raw)
			if !ok {
				continue
			}
			path := fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex)
			if _, found := block["cache_control"]; found {
				return refusal(path)
			}
			for nestedIndex, nested := range arrayOf(block["content"]) {
				if object, ok := objectOf(nested); ok {
					if _, found := object["cache_control"]; found {
						return refusal(fmt.Sprintf("%s.content[%d]", path, nestedIndex))
					}
				}
			}
		}
	}
	return nil
}

func objectOf(raw []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func arrayOf(raw []byte) []json.RawMessage {
	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		return nil
	}
	return array
}

func validateMessages(requestID contract.RequestID, request messagesRequest) *contract.PublicError {
	switch {
	case request.Model.reference == nil && request.Model.level == nil:
		return invalid(requestID, "model", "model is publisher/model[@revision] or a power level")
	case request.MaxTokens == nil || *request.MaxTokens < 1:
		return invalid(requestID, "max_tokens", "max_tokens is required and positive")
	case len(request.Messages) == 0:
		return invalid(requestID, "messages", "messages is at least one message")
	case request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1):
		return invalid(requestID, "temperature", "temperature is between 0 and 1")
	case request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1):
		return invalid(requestID, "top_p", "top_p is between 0 and 1")
	case request.TopK != nil && *request.TopK < 1:
		return invalid(requestID, "top_k", "top_k is positive")
	case len(request.StopSequences) > 8:
		return invalid(requestID, "stop_sequences", "at most 8 stop sequences")
	case request.ToolChoice != nil && len(request.Tools) == 0:
		return invalid(requestID, "tool_choice", "a tool choice requires at least one tool definition")
	case len(request.Tools) > 128:
		return invalid(requestID, "tools", "at most 128 tools")
	}
	for _, sequence := range request.StopSequences {
		if sequence == "" || utf16Length(sequence) > 256 {
			return invalid(requestID, "stop_sequences", "a stop sequence is 1 to 256 characters")
		}
	}
	if user := request.Metadata; user != nil && user.UserID != nil && utf16Length(*user.UserID) > 512 {
		return invalid(requestID, "metadata.user_id", "user_id is at most 512 characters")
	}
	names := make(map[string]struct{}, len(request.Tools))
	for index, tool := range request.Tools {
		param := fmt.Sprintf("tools[%d]", index)
		switch {
		case tool.Type != nil && *tool.Type != "custom":
			return invalid(requestID, param+".type", "only custom tools are supported; server tools are not")
		case tool.Name == "" || utf16Length(tool.Name) > 128:
			return invalid(requestID, param+".name", "a tool name is 1 to 128 characters")
		case tool.InputSchema == nil:
			return invalid(requestID, param+".input_schema", "a tool declares its input_schema")
		case tool.Description != nil && utf16Length(*tool.Description) > 2000:
			return invalid(requestID, param+".description", "a tool description is at most 2000 characters")
		}
		if _, duplicate := names[tool.Name]; duplicate {
			return invalid(requestID, "tools", "tool names must be unique within one request")
		}
		names[tool.Name] = struct{}{}
	}
	return nil
}

// estimatedToolTokens sizes the tool declarations the model reads, content-free
// like estimatedTokens: four bytes a token of each name, description and schema.
func estimatedToolTokens(tools []messagesTool) int {
	total := 0
	for _, tool := range tools {
		size := len(tool.Name)
		if tool.Description != nil {
			size += len(*tool.Description)
		}
		if encoded, err := json.Marshal(tool.InputSchema); err == nil {
			size += len(encoded)
		}
		total += size/4 + 1
	}
	return total
}

// messageParts translates a message's text and image blocks.
type messageParts struct {
	parts []contract.ContentPart
	paths []string
}

func (m *messageParts) add(part contract.ContentPart, path string) {
	m.parts = append(m.parts, part)
	m.paths = append(m.paths, path)
}

// translateMessages turns the system prompt and the conversation into the
// contract's messages. paths holds, for each translated part, where it was in
// the request, so an image refusal names the caller's own location.
func translateMessages(requestID contract.RequestID, request messagesRequest) ([]contract.Message, [][]string, *contract.PublicError) {
	var messages []contract.Message
	var paths [][]string
	if system := request.System; system != nil {
		var parts messageParts
		switch {
		case system.text != nil:
			if *system.text != "" {
				parts.add(textPart(*system.text), "system")
			}
		default:
			for index, block := range system.blocks {
				path := fmt.Sprintf("system[%d]", index)
				if block.Type != "text" || block.Text == nil || !onlyMembers(block, "text") {
					return nil, nil, invalid(requestID, path, "a system block is {type: text, text}")
				}
				parts.add(textPart(*block.Text), path)
			}
		}
		if len(parts.parts) > 0 {
			messages = append(messages, contract.Message{Role: contract.RoleSystem, Content: parts.parts})
			paths = append(paths, parts.paths)
		}
	}
	for index, message := range request.Messages {
		base := fmt.Sprintf("messages[%d]", index)
		role := contract.RoleUser
		switch message.Role {
		case "user":
		case "assistant":
			role = contract.RoleAssistant
		default:
			return nil, nil, invalid(requestID, base+".role", "role is user or assistant; the system prompt is the system parameter")
		}
		if message.Content == nil {
			return nil, nil, invalid(requestID, base+".content", "content is a string or a list of content blocks")
		}
		if message.Content.text != nil {
			text := *message.Content.text
			messages = append(messages, contract.Message{Role: role, Content: []contract.ContentPart{textPart(text)}})
			paths = append(paths, []string{base + ".content"})
			continue
		}
		if len(message.Content.blocks) == 0 {
			return nil, nil, invalid(requestID, base+".content", "content has at least one block")
		}
		current := contract.Message{Role: role, Content: []contract.ContentPart{}}
		var currentPaths []string
		flush := func() {
			if len(current.Content) > 0 || len(current.ToolCalls) > 0 {
				messages = append(messages, current)
				paths = append(paths, currentPaths)
			}
			current, currentPaths = contract.Message{Role: role, Content: []contract.ContentPart{}}, nil
		}
		for blockIndex, block := range message.Content.blocks {
			path := fmt.Sprintf("%s.content[%d]", base, blockIndex)
			switch block.Type {
			case "text":
				if block.Text == nil || !onlyMembers(block, "text") {
					return nil, nil, invalid(requestID, path, "a text block is {type: text, text}")
				}
				current.Content = append(current.Content, textPart(*block.Text))
				currentPaths = append(currentPaths, path)
			case "image":
				part, failure := imagePart(requestID, block, path)
				if failure != nil {
					return nil, nil, failure
				}
				current.Content = append(current.Content, part)
				currentPaths = append(currentPaths, path)
			case "tool_use":
				if role != contract.RoleAssistant {
					return nil, nil, invalid(requestID, path, "a tool_use block is part of an assistant message")
				}
				call, failure := toolCall(requestID, block, path)
				if failure != nil {
					return nil, nil, failure
				}
				current.ToolCalls = append(current.ToolCalls, call)
			case "tool_result":
				if role != contract.RoleUser {
					return nil, nil, invalid(requestID, path, "a tool_result block is part of a user message")
				}
				result, resultPaths, failure := toolResult(requestID, block, path)
				if failure != nil {
					return nil, nil, failure
				}
				flush()
				messages = append(messages, result)
				paths = append(paths, resultPaths)
			case "thinking", "redacted_thinking":
				return nil, nil, invalid(requestID, path, block.Type+" blocks are not supported: Kaana never returns reasoning, so there is none to send back")
			default:
				return nil, nil, invalid(requestID, path, "this content block type is not supported; text, image, tool_use and tool_result are")
			}
		}
		flush()
	}
	return messages, paths, nil
}

func textPart(text string) contract.ContentPart {
	return contract.ContentPart{Type: contract.ContentPartText, Text: &text}
}

// onlyMembers reports whether a block carries no member other than type and
// the named ones, so a member foreign to its type is refused, not dropped.
func onlyMembers(block messagesBlock, allowed ...string) bool {
	present := map[string]bool{
		"text": block.Text != nil, "source": block.Source != nil, "id": block.ID != nil, "name": block.Name != nil,
		"input": len(block.Input) > 0, "tool_use_id": block.ToolUseID != nil, "content": block.Content != nil, "is_error": block.IsError != nil,
	}
	for _, name := range allowed {
		delete(present, name)
	}
	for _, found := range present {
		if found {
			return false
		}
	}
	return true
}

// imagePart carries a base64 image inline. A URL source parses and is refused
// here by name: nothing fetches an image URL (ADR 0007).
func imagePart(requestID contract.RequestID, block messagesBlock, path string) (contract.ContentPart, *contract.PublicError) {
	if block.Source == nil || !onlyMembers(block, "source") {
		return contract.ContentPart{}, invalid(requestID, path, "an image block is {type: image, source}")
	}
	source := block.Source
	switch {
	case source.Type == "url":
		return contract.ContentPart{}, invalid(requestID, path+".source", "an image is sent inline as base64; no image URL is fetched")
	case source.Type != "base64" || source.MediaType == nil || source.Data == nil || source.URL != nil:
		return contract.ContentPart{}, invalid(requestID, path+".source", "an image source is {type: base64, media_type, data}")
	}
	return contract.ContentPart{Type: contract.ContentPartImage, Source: &contract.ContentSource{
		Kind: contract.ContentSourceInline, MediaType: source.MediaType, Data: source.Data}}, nil
}

// toolCall is an assistant's earlier tool_use, with its input as JSON text.
func toolCall(requestID contract.RequestID, block messagesBlock, path string) (contract.ToolCall, *contract.PublicError) {
	switch {
	case block.ID == nil || block.Name == nil || len(block.Input) == 0 || !onlyMembers(block, "id", "name", "input"):
		return contract.ToolCall{}, invalid(requestID, path, "a tool_use block is {type: tool_use, id, name, input}")
	case *block.ID == "" || utf16Length(*block.ID) > 128:
		return contract.ToolCall{}, invalid(requestID, path+".id", "a tool call id is 1 to 128 characters")
	case *block.Name == "" || utf16Length(*block.Name) > 128:
		return contract.ToolCall{}, invalid(requestID, path+".name", "a tool name is 1 to 128 characters")
	}
	var compact bytes.Buffer
	if bytes.TrimSpace(block.Input)[0] != '{' || json.Compact(&compact, block.Input) != nil {
		return contract.ToolCall{}, invalid(requestID, path+".input", "a tool input is a JSON object")
	}
	return contract.ToolCall{ID: *block.ID, Name: *block.Name, Arguments: compact.String()}, nil
}

// toolResult is the tool message answering one tool_use. The contract's tool
// message has no error flag, so is_error is accepted and not forwarded: the
// result's own content is what the model reads.
func toolResult(requestID contract.RequestID, block messagesBlock, path string) (contract.Message, []string, *contract.PublicError) {
	if block.ToolUseID == nil || !onlyMembers(block, "tool_use_id", "content", "is_error") {
		return contract.Message{}, nil, invalid(requestID, path, "a tool_result block is {type: tool_result, tool_use_id, content}")
	}
	if *block.ToolUseID == "" || utf16Length(*block.ToolUseID) > 128 {
		return contract.Message{}, nil, invalid(requestID, path+".tool_use_id", "a tool call id is 1 to 128 characters")
	}
	result := contract.Message{Role: contract.RoleTool, ToolCallID: block.ToolUseID, Content: []contract.ContentPart{}}
	var paths []string
	if content := block.Content; content != nil {
		if content.text != nil {
			result.Content = append(result.Content, textPart(*content.text))
			paths = append(paths, path+".content")
		}
		for index, nested := range content.blocks {
			nestedPath := fmt.Sprintf("%s.content[%d]", path, index)
			switch {
			case nested.Type == "text" && nested.Text != nil && onlyMembers(nested, "text"):
				result.Content = append(result.Content, textPart(*nested.Text))
			case nested.Type == "image":
				part, failure := imagePart(requestID, nested, nestedPath)
				if failure != nil {
					return contract.Message{}, nil, failure
				}
				result.Content = append(result.Content, part)
			default:
				return contract.Message{}, nil, invalid(requestID, nestedPath, "a tool result carries text and image blocks")
			}
			paths = append(paths, nestedPath)
		}
	}
	return result, paths, nil
}

// messagesImageInput applies the contract's image rules (ADR 0007) before
// anything is held and names a refusal at the caller's own location.
func messagesImageInput(requestID contract.RequestID, messages []contract.Message, paths [][]string) (*int, *contract.PublicError) {
	images, issues := contract.CheckImageInput(messages)
	if len(issues) > 0 {
		issue := issues[0]
		if issue.PartIndex == nil {
			return nil, invalid(requestID, "messages", issue.Message)
		}
		param := paths[issue.MessageIndex][*issue.PartIndex]
		switch issue.Field {
		case contract.ImageIssueMediaType:
			param += ".source.media_type"
		case contract.ImageIssueData:
			param += ".source.data"
		case contract.ImageIssueSource:
			param += ".source"
		}
		return nil, invalid(requestID, param, issue.Message)
	}
	if images == 0 {
		return nil, nil
	}
	return &images, nil
}

/* -------------------------------------------------------------------------- */
/*  Streaming                                                                 */
/* -------------------------------------------------------------------------- */

// pendingTool is a tool call being folded until it is complete: Anthropic
// streams blocks one after another, while calls may interleave their deltas.
type pendingTool struct {
	id        string
	name      string
	arguments strings.Builder
	emitted   bool
}

// messagesStream streams Anthropic events: message_start and a ping, text as
// one block at a time, each tool call as one block once complete, then
// message_delta with the stop reason and measured usage, and message_stop. A
// failure after the first frame is one `error` frame and the end. The input
// tokens are known only when the route reports usage, so message_start carries
// zeros and message_delta the measured totals.
func (s *Server) messagesStream(ctx context.Context, w http.ResponseWriter, admitted *admission, envelope *contract.Request) {
	requestID := admitted.requestID
	var writer *sse.Writer
	folded := newCollected()
	var model string
	var next int
	textOpen := false
	tools := make(map[string]*pendingTool)
	var order []*pendingTool
	write := func(name string, payload map[string]any) error {
		if writer == nil {
			opened, err := openSSE(w, requestID)
			if err != nil {
				return err
			}
			writer = opened
		}
		payload["type"] = name
		return writeFrame(writer, name, payload)
	}
	closeText := func() error {
		if !textOpen {
			return nil
		}
		textOpen = false
		return write("content_block_stop", map[string]any{"index": next - 1})
	}
	text := func(words string) error {
		if !textOpen {
			textOpen = true
			next++
			if err := write("content_block_start", map[string]any{"index": next - 1, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
				return err
			}
		}
		return write("content_block_delta", map[string]any{"index": next - 1, "delta": map[string]any{"type": "text_delta", "text": words}})
	}
	emitTool := func(call *pendingTool) error {
		if call.emitted || call.name == "" {
			return nil
		}
		call.emitted = true
		if err := closeText(); err != nil {
			return err
		}
		next++
		index := next - 1
		if err := write("content_block_start", map[string]any{"index": index, "content_block": map[string]any{
			"type": "tool_use", "id": call.id, "name": call.name, "input": map[string]any{}}}); err != nil {
			return err
		}
		if call.arguments.Len() > 0 {
			if err := write("content_block_delta", map[string]any{"index": index, "delta": map[string]any{
				"type": "input_json_delta", "partial_json": call.arguments.String()}}); err != nil {
				return err
			}
		}
		return write("content_block_stop", map[string]any{"index": index})
	}
	result := s.execute(ctx, admitted, envelope, func(event contract.StreamEvent) error {
		if err := folded.fold(event); err != nil {
			return err
		}
		switch e := event.(type) {
		case *contract.StreamStartEvent:
			model = string(e.ResolvedModelReference)
			if err := write("message_start", map[string]any{"message": map[string]any{
				"id": messageID(requestID), "type": "message", "role": "assistant", "model": model, "content": []any{},
				"stop_reason": nil, "stop_sequence": nil, "usage": messagesUsage{}}}); err != nil {
				return err
			}
			return write("ping", map[string]any{})
		case *contract.StreamDeltaEvent:
			if e.Channel == contract.ChannelOutputText || e.Channel == contract.ChannelRefusal {
				return text(e.Text)
			}
		case *contract.StreamToolCallEvent:
			call, known := tools[e.ToolCallID]
			if !known {
				call = &pendingTool{id: e.ToolCallID}
				tools[e.ToolCallID] = call
				order = append(order, call)
			}
			if e.Name != nil {
				call.name = *e.Name
			}
			if e.ArgumentsDelta != nil {
				call.arguments.WriteString(*e.ArgumentsDelta)
			}
			if e.Complete {
				return emitTool(call)
			}
		}
		return nil
	})
	if writer == nil {
		failure := failureOf(result, requestID)
		writeJSON(w, requestID, failure.Code.HTTPStatus(), messagesErrorOf(failure))
		return
	}
	if result.Failure != nil || result.Report == nil {
		_ = write("error", map[string]any{"error": messagesErrorOf(failureOf(result, requestID)).Error, "request_id": string(requestID)})
		return
	}
	for _, call := range order {
		if err := emitTool(call); err != nil {
			return
		}
	}
	if err := closeText(); err != nil {
		return
	}
	if err := write("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": messagesStopReason(finishOr(folded.finish)), "stop_sequence": nil},
		"usage": messagesUsageOf(result.Report.Units)}); err != nil {
		return
	}
	_ = write("message_stop", map[string]any{})
}
