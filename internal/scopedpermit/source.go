// Package scopedpermit binds the private operation to source-reviewed authority.
// It never manufactures a principal, grants scopes or exempts catalogue policy.
package scopedpermit

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

// SourceReviewedAudience remains absent until exact existing principal/funding,
// privacy, rights, ZDR, actual pricing and dated candidate review are complete.
// There is no environment, public constructor or runtime configuration switch.
func SourceReviewedAudience() *contract.ScopedExecutionAudience { return nil }

func Matches(actual, expected *contract.ScopedExecutionAudience, at time.Time) bool {
	return actual != nil && expected != nil && actual.Equal(expected) && actual.Validate() == nil && actual.NotExpired(at) && actual.Provider == "openrouter" && actual.UpstreamModelID == "typesafe/jev-1.13-20260917" && actual.ModelReference == "typesafe/jev-1.13@2026-09-17"
}
