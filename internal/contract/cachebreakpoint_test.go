package contract

import (
	"fmt"
	"slices"
	"testing"
)

func markedText(text string, marked bool) ContentPart {
	part := ContentPart{Type: ContentPartText, Text: &text}
	if marked {
		yes := true
		part.CacheBreakpoint = &yes
	}
	return part
}

// carried lists where the request carries a breakpoint, tools then messages in list order.
func carried(request *Request) []string {
	var found []string
	for _, tool := range request.Tools {
		if marked(tool.CacheBreakpoint) {
			found = append(found, "tool:"+tool.Name)
		}
	}
	for index, message := range request.Input.Messages {
		for part, content := range message.Content {
			if marked(content.CacheBreakpoint) {
				found = append(found, fmt.Sprintf("%d.%s.%d", index, message.Role, part))
			}
		}
	}
	return found
}

func requestWith(tools []ToolDefinition, messages ...Message) *Request {
	return &Request{Tools: tools, Input: Input{Format: InputMessages, Messages: messages}}
}

// The earliest breakpoints in prompt order survive: tools, then instructions
// (even one that comes late in the conversation, because a provider hoists it),
// then the conversation. A message sent as one block takes one slot.
func TestLimitCacheBreakpointsKeepsTheEarliestInPromptOrder(t *testing.T) {
	yes := true
	request := requestWith(
		[]ToolDefinition{{Name: "a", CacheBreakpoint: &yes}, {Name: "b", CacheBreakpoint: &yes}},
		Message{Role: RoleUser, Content: []ContentPart{markedText("u0", true), markedText("u1", true)}},
		Message{Role: RoleSystem, Content: []ContentPart{markedText("s0", true), markedText("s1", true)}},
		Message{Role: RoleTool, Content: []ContentPart{markedText("t0", true), markedText("t1", true)}},
	)
	before := carried(request)

	limited, removed := LimitCacheBreakpoints(request, 4)
	// tool a, tool b, the system message (one slot for both its parts), then the
	// first user part; the second user part and the tool message are over four.
	want := []string{"tool:a", "tool:b", "0.user.0", "1.system.0", "1.system.1"}
	if got := carried(limited); !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
	if removed != 3 {
		t.Errorf("removed %d, want the second user part and the tool message's two (3)", removed)
	}
	if got := carried(request); !slices.Equal(got, before) {
		t.Errorf("the request was modified: %v, was %v", got, before)
	}

	// Exactly at the limit nothing is removed, and the request comes back as it came.
	same, removed := LimitCacheBreakpoints(requestWith(nil, Message{Role: RoleUser, Content: []ContentPart{markedText("a", true), markedText("b", true), markedText("c", true), markedText("d", true)}}), MaxCacheBreakpoints)
	if removed != 0 || len(carried(same)) != 4 {
		t.Errorf("four at a limit of four: kept %d, removed %d", len(carried(same)), removed)
	}
	none, removed := LimitCacheBreakpoints(request, 0)
	if removed != len(before) || len(carried(none)) != 0 {
		t.Errorf("a limit of 0 kept %v and removed %d of %d", carried(none), removed, len(before))
	}
}

// A marker on a part no provider caches is removed whatever the limit, and a
// request without any is returned untouched.
func TestLimitCacheBreakpointsRemovesWhatCannotBeCached(t *testing.T) {
	yes := true
	reasoning := ContentPart{Type: ContentPartReasoning, Text: new("hm"), CacheBreakpoint: &yes}
	request := requestWith(nil, Message{Role: RoleAssistant, Content: []ContentPart{reasoning, markedText("", true), markedText("real", true)}})
	limited, removed := LimitCacheBreakpoints(request, 4)
	if got := carried(limited); !slices.Equal(got, []string{"0.assistant.2"}) || removed != 2 {
		t.Errorf("kept %v, removed %d; only the non-empty text may carry one", got, removed)
	}

	plain := requestWith(nil, Message{Role: RoleUser, Content: []ContentPart{markedText("a", false)}})
	if same, removed := LimitCacheBreakpoints(plain, 4); same != plain || removed != 0 {
		t.Error("a request without a breakpoint was copied or changed")
	}
}
