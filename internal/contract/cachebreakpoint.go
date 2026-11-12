package contract

// MaxCacheBreakpoints is the most prompt-cache breakpoints a request may carry
// to a provider: Anthropic's documented limit, the strictest one a route has.
const MaxCacheBreakpoints = 4

// breakpointCarrier is whether a part can carry a breakpoint at all: text with
// something in it, an image or a file. Reasoning, refusal and audio parts and
// empty text cannot be cached by any provider that takes the marker.
func breakpointCarrier(part ContentPart) bool {
	switch part.Type {
	case ContentPartText:
		return part.Text != nil && *part.Text != ""
	case ContentPartImage, ContentPartFile:
		return true
	default:
		return false
	}
}

func marked(flag *bool) bool { return flag != nil && *flag }

// HasCacheBreakpoints reports whether the request carries a breakpoint
// anywhere, on a tool or on a part.
func HasCacheBreakpoints(request *Request) bool {
	for _, tool := range request.Tools {
		if marked(tool.CacheBreakpoint) {
			return true
		}
	}
	if request.Input.Format != InputMessages {
		return false
	}
	for _, message := range request.Input.Messages {
		for _, part := range message.Content {
			if marked(part.CacheBreakpoint) {
				return true
			}
		}
	}
	return false
}

// LimitCacheBreakpoints returns the request with at most limit breakpoints and
// how many it removed; limit 0 removes them all. Slots are taken in the order a
// provider that hoists instructions reads the prompt: tools (one each), then
// system and developer messages, then the conversation. A system, developer or
// tool message is sent as one block, so it takes one slot however many of its
// parts are marked; a user or assistant part takes one each. The earliest
// breakpoints are kept. A marker on a part no provider can cache is removed
// whatever the limit.
//
// The request is not modified. When nothing is removed it is returned as it
// came; otherwise a copy that shares everything but the slices it edits.
func LimitCacheBreakpoints(request *Request, limit int) (*Request, int) {
	if !HasCacheBreakpoints(request) {
		return request, 0
	}
	kept, removed := 0, 0
	take := func() bool {
		if kept < limit {
			kept++
			return true
		}
		return false
	}
	out := *request

	if len(request.Tools) > 0 {
		out.Tools = append([]ToolDefinition(nil), request.Tools...)
		for index := range out.Tools {
			if marked(out.Tools[index].CacheBreakpoint) && !take() {
				out.Tools[index].CacheBreakpoint = nil
				removed++
			}
		}
	}
	if request.Input.Format != InputMessages {
		return &out, removed
	}

	out.Input.Messages = make([]Message, len(request.Input.Messages))
	for index, message := range request.Input.Messages {
		message.Content = append([]ContentPart(nil), message.Content...)
		out.Input.Messages[index] = message
	}
	drop := func(part *ContentPart) {
		part.CacheBreakpoint = nil
		removed++
	}
	// A message sent as one block takes one slot for all its marked parts.
	settleBlock := func(message *Message) {
		carried := false
		for index := range message.Content {
			part := &message.Content[index]
			switch {
			case !marked(part.CacheBreakpoint):
			case breakpointCarrier(*part):
				carried = true
			default:
				drop(part)
			}
		}
		if carried && !take() {
			for index := range message.Content {
				if marked(message.Content[index].CacheBreakpoint) {
					drop(&message.Content[index])
				}
			}
		}
	}
	settleParts := func(message *Message) {
		for index := range message.Content {
			part := &message.Content[index]
			if marked(part.CacheBreakpoint) && (!breakpointCarrier(*part) || !take()) {
				drop(part)
			}
		}
	}
	// Instructions first: they are hoisted ahead of the conversation.
	for index := range out.Input.Messages {
		if role := out.Input.Messages[index].Role; role == RoleSystem || role == RoleDeveloper {
			settleBlock(&out.Input.Messages[index])
		}
	}
	for index := range out.Input.Messages {
		message := &out.Input.Messages[index]
		switch message.Role {
		case RoleSystem, RoleDeveloper:
		case RoleTool:
			settleBlock(message)
		default:
			settleParts(message)
		}
	}
	return &out, removed
}
