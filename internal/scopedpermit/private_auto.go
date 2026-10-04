package scopedpermit

import "github.com/OxyHQ/Kaana/internal/contract"

// Private Auto has independent source authority. The synthetic commissioning
// audience cannot grant it. No environment or wire field can activate this.
func SourceReviewedPrivateAutoApproval() *contract.PrivateAutoSourceApproval { return nil }
