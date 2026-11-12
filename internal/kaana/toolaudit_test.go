package kaana

import (
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func auditFor(names ...string) *toolCallAudit {
	tools := make([]contract.ToolDefinition, 0, len(names))
	for _, name := range names {
		tools = append(tools, contract.ToolDefinition{Type: "function", Name: name})
	}
	return newToolCallAudit(tools)
}

func TestToolCallAuditJudgesCompletedCalls(t *testing.T) {
	cases := []struct {
		name      string
		feed      func(*toolCallAudit)
		completed int
		malformed int
	}{
		{"a declared tool with object arguments is well formed", func(a *toolCallAudit) {
			a.observe("c1", "get_weather", `{"city":`, false)
			a.observe("c1", "", `"Oslo"}`, true)
		}, 1, 0},
		{"no-argument calls are well formed however they are spelled", func(a *toolCallAudit) {
			a.observe("c1", "get_weather", "", true)
			a.observe("c2", "get_weather", "{}", true)
		}, 2, 0},
		{"a tool the request did not declare is malformed", func(a *toolCallAudit) {
			a.observe("c1", "delete_everything", `{}`, true)
		}, 1, 1},
		{"a call with no name is malformed", func(a *toolCallAudit) {
			a.observe("c1", "", `{}`, true)
		}, 1, 1},
		{"arguments that are not JSON are malformed", func(a *toolCallAudit) {
			a.observe("c1", "get_weather", `{"city": "Oslo"`, true)
		}, 1, 1},
		{"a call cut before it completes is not judged", func(a *toolCallAudit) {
			a.observe("c1", "get_weather", `{"city": "Os`, false)
		}, 0, 0},
		{"interleaved calls are judged separately", func(a *toolCallAudit) {
			a.observe("c1", "get_weather", `{"a":`, false)
			a.observe("c2", "get_weather", `not json`, true)
			a.observe("c1", "", `1}`, true)
		}, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := auditFor("get_weather")
			tc.feed(audit)
			if audit.completed != tc.completed || audit.malformed != tc.malformed {
				t.Fatalf("completed/malformed = %d/%d, want %d/%d", audit.completed, audit.malformed, tc.completed, tc.malformed)
			}
		})
	}
}

func TestToolCallAuditDoesNotBlameTheBoundForAHugeCall(t *testing.T) {
	audit := auditFor("get_weather")
	audit.observe("c1", "get_weather", `{"x":"`+strings.Repeat("a", maxAuditedArgumentBytes), false)
	audit.observe("c1", "", `"}`, true)
	if audit.completed != 1 || audit.malformed != 0 {
		t.Fatalf("an oversized call must be unjudged, not malformed: %d/%d", audit.completed, audit.malformed)
	}
}

func TestToolCallAuditForgetsAnAttemptOnReset(t *testing.T) {
	audit := auditFor("get_weather")
	audit.observe("c1", "nope", `{}`, true)
	audit.observe("c2", "get_weather", `{`, false)
	audit.reset()
	if audit.completed != 0 || audit.malformed != 0 || len(audit.open) != 0 {
		t.Fatalf("a failover inherited the failed attempt's calls: %+v", audit)
	}
}
