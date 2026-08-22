package contract

import "encoding/json"

// BillingPrincipal is the financially responsible principal, and the only
// identity a charge may be booked against.
//
// It is its own type rather than a field on a larger principal object for the
// same reason the contract makes it one: a function taking "who pays" cannot
// then be handed a user, a session, a device or an application.
type BillingPrincipal struct {
	AccountID AccountID `json:"accountId"`
}

// AuthenticatedPrincipal is who authenticated, as resolved by the Oxy edge
// before a request is forwarded.
//
// Pensara authorizes nothing about a customer against it. It has no account graph
// to re-derive access from, and re-deriving would reintroduce exactly the
// replication lag that makes revocation unsafe (ADR 0006).
type AuthenticatedPrincipal struct {
	Billing         BillingPrincipal `json:"billing"`
	ApplicationID   ApplicationID    `json:"applicationId"`
	CredentialID    CredentialID     `json:"credentialId"`
	Environment     Environment      `json:"environment"`
	InferenceScopes []Scope          `json:"inferenceScopes"`
}

// MarshalJSON renders a principal holding no inference scopes as
// `"inferenceScopes": []`, for the same reason UsageReport.MarshalJSON exists:
// the contract's array has no null spelling, and Go's zero slice encodes as one.
//
// Pensara refuses an envelope without inference:invoke before it can produce a
// report, so no live path emits an empty list today. It is fixed here anyway
// because the field rides on the same report through the same encoder, and a
// fix applied only to the sibling that happens to be reachable is how the same
// failure lands again the next time an envelope's shape changes.
func (p AuthenticatedPrincipal) MarshalJSON() ([]byte, error) {
	type wire AuthenticatedPrincipal
	encoded := wire(p)
	if encoded.InferenceScopes == nil {
		encoded.InferenceScopes = []Scope{}
	}
	return json.Marshal(encoded)
}

// Attribution is the block carried by every request, event and usage report.
//
// UserID is the optional delegated end user. It is attribution only: it never
// changes which account is charged, and it lives outside BillingPrincipal so no
// code path can read it as the payer.
type Attribution struct {
	Principal    AuthenticatedPrincipal `json:"principal"`
	UserID       *UserID                `json:"userId,omitempty"`
	RequestID    RequestID              `json:"requestId"`
	GenerationID *GenerationID          `json:"generationId,omitempty"`
}

// HasScope reports whether the authenticated principal carries a scope.
//
// Pensara uses this for one thing only — refusing an envelope that was never
// authorized to invoke inference at all, which is a malformed instruction from
// the edge rather than a customer authorization decision. Everything a customer
// can be told "no" about is decided at the edge, before forwarding.
func (a Attribution) HasScope(scope Scope) bool {
	for _, granted := range a.Principal.InferenceScopes {
		if granted == scope {
			return true
		}
	}
	return false
}
