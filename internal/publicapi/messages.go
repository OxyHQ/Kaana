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
	body, ignored := stripIgnored(body)
	if len(ignored) > 0 {
		w.Header().Set(HeaderIgnoredParameters, strings.Join(ignored, ", "))
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
	reasoning, failure := messagesReasoning(requestID, request)
	if failure != nil {
		refuse(failure)
		return
	}
	stream := request.Stream != nil && *request.Stream
	estimate := estimatedTokens(messages) + estimatedToolTokens(request.Tools)
	var choice *contract.ToolChoice
	if request.ToolChoice != nil {
		choice = &request.ToolChoice.choice
	}
	admitted, failure := s.admit(r.Context(), r, requestID, receivedAt, authorization{
		operation: contract.GrantResponses, target: request.Model.grantTarget(), body: body, endpoint: "/v1/messages",
		facts: contract.GrantRequestFacts{EstimatedInputTokens: &estimate, MaxOutputTokens: request.MaxTokens,
			InputImages: images, DeclaredTools: len(request.Tools), Stream: stream},
		needs: requestNeeds{parameters: carriedParameters(request.MaxTokens, contract.SamplingParameters{Temperature: request.Temperature, TopP: request.TopP, TopK: request.TopK,
			StopSequences: request.StopSequences}, len(request.Tools), choice, nil, reasoning)},
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
	envelope.Reasoning = reasoning
	if stream {
		s.messagesStream(r.Context(), w, admitted, &envelope)
		return
	}
	folded := newCollected()
	folded.keepReasoning = reasoning != nil && !reasoning.Excluded()
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

// messagesReasoning is the contract's reasoning control a request asks for:
// `output_config.effort` is the effort, `thinking: {type: enabled,
// budget_tokens}` the token budget and `thinking: {type: adaptive}` the opt-in
// with the route's own sizing. An effort and a budget are alternatives, as in
// the contract, and are refused together rather than one being dropped.
func messagesReasoning(requestID contract.RequestID, request messagesRequest) (*contract.ReasoningParameters, *contract.PublicError) {
	var reasoning *contract.ReasoningParameters
	if request.OutputConfig != nil && request.OutputConfig.Effort != nil {
		reasoning = &contract.ReasoningParameters{Effort: request.OutputConfig.Effort}
	}
	if request.Thinking == nil || !request.Thinking.enabled {
		return reasoning, nil
	}
	if reasoning == nil {
		reasoning = &contract.ReasoningParameters{}
	}
	if request.Thinking.budget != nil {
		if reasoning.Effort != nil {
			return nil, invalid(requestID, "thinking.budget_tokens", "a thinking budget and output_config.effort are alternatives; send one")
		}
		reasoning.MaxTokens = request.Thinking.budget
	}
	return reasoning, nil
}

// messageID is the id of the message: the request id in Anthropic's spelling.
func messageID(requestID contract.RequestID) string {
	return "msg_" + strings.TrimPrefix(string(requestID), "req_")
}

// messagesContent folds an answer into its output blocks: its words, a
// refusal's words, then each tool call.
func messagesContent(requestID contract.RequestID, folded *collected) ([]messagesOutputBlock, *contract.PublicError) {
	content := []messagesOutputBlock{}
	if folded.keepReasoning {
		for _, part := range folded.reasoningParts() {
			thinking, signature := "", ""
			if part.Text != nil {
				thinking = *part.Text
			}
			if part.Opaque != nil {
				signature = *part.Opaque
			}
			content = append(content, messagesOutputBlock{Type: "thinking", Thinking: &thinking, Signature: &signature})
		}
	}
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

// Members of the Anthropic dialect that Kaana accepts and does not honour.
// Each is a hint or a beta feature that changes how a provider caches, trims or
// loads, never what the request means or what it costs, and Claude Code sends
// them on every request: refusing them made the endpoint unusable by its main
// client. They are removed before the strict decode, and the answer names every
// one that was present in Kaana-Ignored-Parameters so nothing is left to inference.
const (
	ignoredCacheControl      = "cache_control"
	ignoredContextManagement = "context_management"
	ignoredThinkingDisplay   = "thinking.display"
	ignoredToolDeferLoading  = "tools.defer_loading"
)

// stripIgnored removes the accepted-and-ignored members from a request body and
// returns the body and the names that were present, in a fixed order.
//
// `cache_control` is removed wherever the dialect puts it: the request,
// `system`, a tool, a content block and a tool result's blocks. It is not read
// inside a tool's `input` or `input_schema`, where a key of that name is the
// caller's own data. Because Kaana forwards no cache hint, no cache write
// happens and the usage it reports stays exactly what the request cost; honouring
// it needs a priced cache-write unit nothing emits yet (issue #269).
func stripIgnored(body []byte) ([]byte, []string) {
	root, ok := objectOf(body)
	if !ok {
		return body, nil
	}
	found := map[string]bool{}
	drop := func(raw json.RawMessage, key, name string) json.RawMessage {
		object, ok := objectOf(raw)
		if !ok {
			return raw
		}
		if _, present := object[key]; !present {
			return raw
		}
		delete(object, key)
		found[name] = true
		out, _ := json.Marshal(object)
		return out
	}
	marker := func(raw json.RawMessage) json.RawMessage { return drop(raw, "cache_control", ignoredCacheControl) }
	each := func(raw json.RawMessage, visit func(json.RawMessage) json.RawMessage) json.RawMessage {
		items := arrayOf(raw)
		if items == nil {
			return raw
		}
		for index := range items {
			items[index] = visit(items[index])
		}
		out, _ := json.Marshal(items)
		return out
	}
	withNested := func(raw json.RawMessage, key string, visit func(json.RawMessage) json.RawMessage) json.RawMessage {
		object, ok := objectOf(raw)
		if !ok {
			return raw
		}
		nested, present := object[key]
		if !present || arrayOf(nested) == nil {
			return raw
		}
		object[key] = each(nested, visit)
		out, _ := json.Marshal(object)
		return out
	}
	block := func(raw json.RawMessage) json.RawMessage { return withNested(marker(raw), "content", marker) }
	tool := func(raw json.RawMessage) json.RawMessage {
		return drop(marker(raw), "defer_loading", ignoredToolDeferLoading)
	}
	if _, present := root["cache_control"]; present {
		delete(root, "cache_control")
		found[ignoredCacheControl] = true
	}
	if _, present := root["context_management"]; present {
		delete(root, "context_management")
		found[ignoredContextManagement] = true
	}
	if thinking, present := root["thinking"]; present {
		root["thinking"] = drop(thinking, "display", ignoredThinkingDisplay)
	}
	if system, present := root["system"]; present {
		root["system"] = each(system, marker)
	}
	if tools, present := root["tools"]; present {
		root["tools"] = each(tools, tool)
	}
	if messages, present := root["messages"]; present {
		root["messages"] = each(messages, func(raw json.RawMessage) json.RawMessage {
			return withNested(raw, "content", block)
		})
	}
	var names []string
	for _, name := range []string{ignoredCacheControl, ignoredContextManagement, ignoredThinkingDisplay, ignoredToolDeferLoading} {
		if found[name] {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, names
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
		case tool.Description != nil && utf16Length(*tool.Description) > 32768:
			return invalid(requestID, param+".description", "a tool description is at most 32768 characters")
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
		case "system":
			// A mid-conversation instruction (Claude Code sends one). It stays
			// where it was sent and carries text only; the Anthropic adapter
			// hoists it into the provider's system prompt like any system message.
			role = contract.RoleSystem
		default:
			return nil, nil, invalid(requestID, base+".role", "role is user, assistant or system")
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
			if role == contract.RoleSystem && block.Type != "text" {
				return nil, nil, invalid(requestID, path, "a system message carries text blocks only")
			}
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
				if role != contract.RoleAssistant {
					return nil, nil, invalid(requestID, path, "a "+block.Type+" block is part of an assistant message")
				}
				part, failure := reasoningPart(requestID, block, path)
				if failure != nil {
					return nil, nil, failure
				}
				current.Content = append(current.Content, part)
				currentPaths = append(currentPaths, path)
			default:
				return nil, nil, invalid(requestID, path, "this content block type is not supported; text, image, tool_use and tool_result are")
			}
		}
		flush()
	}
	return messages, paths, nil
}

// reasoningPart is a thinking or redacted_thinking block as the contract's
// reasoning part: the signature (or the redacted data) is the opaque token,
// returned exactly as this endpoint issued it.
func reasoningPart(requestID contract.RequestID, block messagesBlock, path string) (contract.ContentPart, *contract.PublicError) {
	part := contract.ContentPart{Type: contract.ContentPartReasoning}
	if block.Type == "redacted_thinking" {
		if block.Data == nil || *block.Data == "" || !onlyMembers(block, "data") {
			return part, invalid(requestID, path, "a redacted_thinking block is {type: redacted_thinking, data}")
		}
		part.Opaque = block.Data
		return part, nil
	}
	if block.Thinking == nil || block.Signature == nil || !onlyMembers(block, "thinking", "signature") {
		return part, invalid(requestID, path, "a thinking block is {type: thinking, thinking, signature}")
	}
	if *block.Thinking != "" {
		part.Text = block.Thinking
	}
	if *block.Signature != "" {
		part.Opaque = block.Signature
	}
	if part.Text == nil && part.Opaque == nil {
		return part, invalid(requestID, path, "a thinking block carries thinking text or a signature")
	}
	return part, nil
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
		"thinking": block.Thinking != nil, "signature": block.Signature != nil, "data": block.Data != nil,
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
	showReasoning := envelope.Reasoning != nil && !envelope.Reasoning.Excluded()
	var model string
	var next int
	textOpen := false
	thinkingOpen := false
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
	closeThinking := func() error {
		if !thinkingOpen {
			return nil
		}
		thinkingOpen = false
		return write("content_block_stop", map[string]any{"index": next - 1})
	}
	// thinking streams one reasoning block: its words as thinking_delta, then
	// its continuation token as the signature_delta that closes it. A block the
	// route could not seal ends without one.
	thinking := func(e *contract.StreamDeltaEvent) error {
		if err := closeText(); err != nil {
			return err
		}
		if !thinkingOpen {
			thinkingOpen = true
			next++
			if err := write("content_block_start", map[string]any{"index": next - 1, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}); err != nil {
				return err
			}
		}
		if e.Text != "" {
			if err := write("content_block_delta", map[string]any{"index": next - 1, "delta": map[string]any{"type": "thinking_delta", "thinking": e.Text}}); err != nil {
				return err
			}
		}
		if e.Opaque == nil {
			return nil
		}
		if err := write("content_block_delta", map[string]any{"index": next - 1, "delta": map[string]any{"type": "signature_delta", "signature": *e.Opaque}}); err != nil {
			return err
		}
		return closeThinking()
	}
	text := func(words string) error {
		if err := closeThinking(); err != nil {
			return err
		}
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
		if err := closeThinking(); err != nil {
			return err
		}
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
			if e.Channel == contract.ChannelReasoning && showReasoning {
				return thinking(e)
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
	if err := closeThinking(); err != nil {
		return
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
