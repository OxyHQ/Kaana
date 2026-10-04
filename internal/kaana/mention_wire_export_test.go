package kaana

import "github.com/OxyHQ/Kaana/internal/contract"

// SetMentionAudienceForTest exists only in Go's test binary. Production still
// obtains its audience from compiled source, never from a fixture file.
func SetMentionAudienceForTest(e *Executor, audience contract.ScopedExecutionAudience) {
	e.scopedSource = func() *contract.ScopedExecutionAudience { return &audience }
}

// Test binary only; no production activation switch is introduced.
func SetPrivateAutoApprovalForTest(e *Executor, approval contract.PrivateAutoSourceApproval) {
	e.privateAutoSource = func() *contract.PrivateAutoSourceApproval { return &approval }
}
