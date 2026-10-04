package kaana

import "github.com/OxyHQ/Kaana/internal/contract"

// SetMentionAudienceForTest exists only in Go's test binary. Production still
// obtains its audience from the nil source-reviewed getter, never from a file.
func SetMentionAudienceForTest(e *Executor, audience contract.ScopedExecutionAudience) {
	e.scopedSource = func() *contract.ScopedExecutionAudience { return &audience }
}
