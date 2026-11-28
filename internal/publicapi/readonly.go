package publicapi

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/pgerror"
)

// readOnlyRefusal answers a request Kaana could not admit because its database
// refused the write as read-only (SQLSTATE 25006), as it does during a planned
// freeze. Nothing was admitted or executed, so the caller is told to retry
// after pgerror.ReadOnlyRetryAfter rather than handed an internal error.
func readOnlyRefusal(requestID contract.RequestID) *contract.PublicError {
	return contract.NewPublicError(requestID, "service_unavailable", contract.PublicMessage("service_unavailable")).
		WithRetryAfter(int(pgerror.ReadOnlyRetryAfter.Milliseconds()))
}
