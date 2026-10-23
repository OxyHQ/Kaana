package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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
// Kaana authorizes nothing about a customer against it. It has no account graph
// to re-derive access from, and re-deriving would reintroduce exactly the
// replication lag that makes revocation unsafe (ADR 0006).
type ServiceAuthority string

const (
	AuthorityFirstParty ServiceAuthority = "first_party"
	AuthorityExternal   ServiceAuthority = "external"
)

var serviceAuthorityValues = []ServiceAuthority{AuthorityFirstParty, AuthorityExternal}

// nil means the legacy external contract, never internal authority.
func isFirstParty(authority *ServiceAuthority) bool {
	return authority != nil && *authority == AuthorityFirstParty
}

func validateAuthority(authority *ServiceAuthority, scopes []Scope) error {
	if authority != nil && !isMember(*authority, serviceAuthorityValues) {
		return fmt.Errorf("contract: unknown service authority %q", *authority)
	}
	for _, scope := range scopes {
		if !isMember(scope, scopeValues) {
			return fmt.Errorf("contract: unknown external inference scope %q", scope)
		}
	}
	if isFirstParty(authority) && scopes != nil {
		return fmt.Errorf("contract: first-party instructions do not carry scopes")
	}
	return nil
}

type AuthenticatedPrincipal struct {
	Authority       *ServiceAuthority `json:"authority,omitempty"`
	Billing         BillingPrincipal  `json:"billing"`
	ApplicationID   ApplicationID     `json:"applicationId"`
	CredentialID    CredentialID      `json:"credentialId"`
	Environment     Environment       `json:"environment"`
	InferenceScopes []Scope           `json:"inferenceScopes,omitempty"`
}

// MarshalJSON preserves legacy external arrays and omits scopes for own workloads.
func (p AuthenticatedPrincipal) MarshalJSON() ([]byte, error) {
	if err := validateAuthority(p.Authority, p.InferenceScopes); err != nil {
		return nil, err
	}
	type wire AuthenticatedPrincipal
	encoded := struct {
		wire
		Scopes *[]Scope `json:"inferenceScopes,omitempty"`
	}{wire: wire(p)}
	if !p.IsFirstParty() {
		scopes := p.InferenceScopes
		if scopes == nil {
			scopes = []Scope{}
		}
		encoded.Scopes = &scopes
	}
	return json.Marshal(encoded)
}

func (p *AuthenticatedPrincipal) UnmarshalJSON(data []byte) error {
	type wire AuthenticatedPrincipal
	var decoded wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := validateAuthorityWire(data, decoded.Authority, "inferenceScopes"); err != nil {
		return err
	}
	*p = AuthenticatedPrincipal(decoded)
	return validateAuthority(p.Authority, p.InferenceScopes)
}

func validateAuthorityWire(data []byte, authority *ServiceAuthority, scopeField string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if value, present := fields["authority"]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("contract: service authority cannot be null")
	}
	value, present := fields[scopeField]
	if isFirstParty(authority) {
		if present {
			return fmt.Errorf("contract: first-party instructions cannot carry %s", scopeField)
		}
	} else if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("contract: external instructions require %s", scopeField)
	}
	return nil
}

func (p AuthenticatedPrincipal) IsFirstParty() bool { return isFirstParty(p.Authority) }

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

// HasScope reads recorded EXTERNAL scopes. Internal authority is handled separately.
func (a Attribution) HasScope(scope Scope) bool {
	for _, granted := range a.Principal.InferenceScopes {
		if granted == scope {
			return true
		}
	}
	return false
}
