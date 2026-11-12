package kaana

import (
	"encoding/json"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// maxAuditedArgumentBytes bounds what the audit keeps of one tool call's
// arguments. A call larger than this is not judged rather than judged on a
// prefix: a truncated document is not valid JSON, and saying so would blame the
// deployment for the bound.
const maxAuditedArgumentBytes = 1 << 20

// toolCallAudit judges whether the tool calls an attempt produced are well
// formed enough to act on: the call names a tool the request declared, and its
// arguments are one JSON value. It is the signal a router needs to prefer a
// deployment whose tool calls work (OpenRouter's "Auto Exacto" measures the
// same thing), and it costs no extra request.
//
// It judges shape, not meaning. A call whose arguments are valid JSON but do not
// satisfy the tool's schema passes: checking a schema needs a validator Kaana
// does not carry, and a deployment is not blamed for what the audit cannot see.
//
// A call that never completes is not judged. A stream cut by the caller's
// `max_tokens` leaves a half-written call, and blaming the deployment for the
// caller's limit would teach a router to avoid the models that think longest.
//
// The arguments are customer data. They are held in memory only until the call
// completes, are never logged or stored, and the audit keeps a verdict, not the
// text.
type toolCallAudit struct {
	declared map[string]struct{}
	open     map[string]*auditedCall
	// completed counts calls that finished, malformed counts those that were
	// not well formed.
	completed, malformed int
}

type auditedCall struct {
	name      string
	arguments strings.Builder
	oversized bool
}

func newToolCallAudit(tools []contract.ToolDefinition) *toolCallAudit {
	declared := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		declared[tool.Name] = struct{}{}
	}
	return &toolCallAudit{declared: declared, open: make(map[string]*auditedCall)}
}

// reset forgets the attempt just ended, so a failover is judged on its own
// calls.
func (a *toolCallAudit) reset() {
	clear(a.open)
	a.completed, a.malformed = 0, 0
}

// observe takes one tool-call event.
func (a *toolCallAudit) observe(id, name, argumentsDelta string, complete bool) {
	call := a.open[id]
	if call == nil {
		call = &auditedCall{}
		a.open[id] = call
	}
	if name != "" {
		call.name = name
	}
	if !call.oversized {
		if call.arguments.Len()+len(argumentsDelta) > maxAuditedArgumentBytes {
			call.oversized = true
			call.arguments.Reset()
		} else {
			call.arguments.WriteString(argumentsDelta)
		}
	}
	if !complete {
		return
	}
	delete(a.open, id)
	a.completed++
	if !a.wellFormed(call) {
		a.malformed++
	}
}

func (a *toolCallAudit) wellFormed(call *auditedCall) bool {
	if call.name == "" {
		return false
	}
	if _, ok := a.declared[call.name]; !ok {
		return false
	}
	if call.oversized {
		return true
	}
	arguments := strings.TrimSpace(call.arguments.String())
	// A tool that takes no arguments is called with nothing by some providers
	// and with `{}` by others; both are well formed.
	return arguments == "" || json.Valid([]byte(arguments))
}
