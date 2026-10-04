package kaana

import "github.com/OxyHQ/Kaana/internal/contract"

// SetMentionAudienceForTest exists only in Go's test binary. Production still
// obtains its audience from compiled source, never from a fixture file.
func SetMentionAudienceForTest(e *Executor, audience contract.ScopedExecutionAudience) {
	e.scopedSource = func() *contract.ScopedExecutionAudience { return &audience }
}
