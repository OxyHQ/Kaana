package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

const IdentitySignatureDomain = "oxy-kaana-identity:v1"
const IdentityValidityWindow = time.Minute
const OxyKaanaIdentitiesEndpoint = "/internal/kaana/v1/identities"

// IdentityRequest carries authentication inputs only, never inference facts.
type IdentityRequest struct {
	SchemaVersion    int       `json:"schemaVersion"`
	Kind             string    `json:"kind"`
	RequestID        RequestID `json:"requestId"`
	Audience         string    `json:"audience"`
	Credential       string    `json:"credential"`
	OnBehalfOfUserID *UserID   `json:"onBehalfOfUserId,omitempty"`
}

// IdentityPrincipal is current identity, not an economic authorization.
type IdentityLane string

const IdentityLaneMachine IdentityLane = "machine_credential"
const IdentityLaneService IdentityLane = "service_token"

var identityLaneValues = []IdentityLane{IdentityLaneMachine, IdentityLaneService}

type IdentityApplicationClass string

var identityApplicationClassValues = []IdentityApplicationClass{"first_party", "third_party", "internal", "system"}

type IdentityPrincipal struct {
	OwnerAccountID        AccountID                `json:"ownerAccountId"`
	ApplicationID         ApplicationID            `json:"applicationId"`
	CredentialID          CredentialID             `json:"credentialId"`
	Environment           Environment              `json:"environment"`
	Lane                  IdentityLane             `json:"lane"`
	Authority             ServiceAuthority         `json:"authority"`
	ApplicationClass      IdentityApplicationClass `json:"applicationClass"`
	ApplicationIsInternal bool                     `json:"applicationIsInternal"`
	Scopes                []Scope                  `json:"scopes,omitempty"`
}

func (p IdentityPrincipal) MarshalJSON() ([]byte, error) {
	type wire IdentityPrincipal
	v := struct {
		wire
		Scopes *[]Scope `json:"scopes,omitempty"`
	}{wire: wire(p)}
	if p.Authority == AuthorityExternal {
		scopes := p.Scopes
		if scopes == nil {
			scopes = []Scope{}
		}
		v.Scopes = &scopes
	}
	return json.Marshal(v)
}

func (p *IdentityPrincipal) UnmarshalJSON(data []byte) error {
	type wire IdentityPrincipal
	var v wire
	if err := identityDecode(data, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if value, ok := fields["applicationIsInternal"]; !ok || string(value) == "null" {
		return errors.New("contract: applicationIsInternal must be stated")
	}
	if err := validateAuthorityWire(data, &v.Authority, "scopes"); err != nil {
		return err
	}
	*p = IdentityPrincipal(v)
	return p.Validate()
}

func (p IdentityPrincipal) Validate() error {
	if !identityID(string(p.OwnerAccountID), 64) || !identityID(string(p.ApplicationID), 64) || !identityID(string(p.CredentialID), 64) || !isMember(p.Environment, environmentValues) {
		return errors.New("contract: identity principal is incomplete")
	}
	if p.Lane != IdentityLaneService && p.Lane != IdentityLaneMachine {
		return errors.New("contract: identity lane is unsupported")
	}
	if !slices.Contains(identityApplicationClassValues, p.ApplicationClass) {
		return errors.New("contract: identity application class is unsupported")
	}
	if p.Authority != AuthorityFirstParty && p.Authority != AuthorityExternal {
		return errors.New("contract: identity authority is explicit")
	}
	if p.Authority == AuthorityFirstParty && p.Lane != IdentityLaneService {
		return errors.New("contract: first-party identity requires a workload")
	}
	return validateAuthority(&p.Authority, p.Scopes)
}

type IdentityAssertion struct {
	SchemaVersion int               `json:"schemaVersion"`
	Kind          string            `json:"kind"`
	RequestID     RequestID         `json:"requestId"`
	Audience      string            `json:"audience"`
	RequestSHA256 string            `json:"requestSha256"`
	IssuedAt      Timestamp         `json:"issuedAt"`
	ExpiresAt     Timestamp         `json:"expiresAt"`
	Principal     IdentityPrincipal `json:"principal"`
	// UserID is attribution, not proof of a person's data authority.
	UserID *UserID `json:"userId,omitempty"`
}

func DecodeIdentityAssertion(data []byte) (*IdentityAssertion, error) {
	var v IdentityAssertion
	if err := identityDecode(data, &v); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if value, ok := fields["userId"]; ok && string(value) == "null" {
		return nil, errors.New("contract: identity attribution is not nullable")
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return &v, nil
}

func (a IdentityAssertion) Validate() error {
	if a.SchemaVersion != 1 || a.Kind != "identity" || a.Audience != "kaana.ai" || !identityID(string(a.RequestID), 128) || !ValidSHA256Hex(a.RequestSHA256) {
		return errors.New("contract: identity assertion binding is invalid")
	}
	issued, err := identityTime(a.IssuedAt)
	if err != nil {
		return errors.New("contract: identity issuedAt is invalid")
	}
	expires, err := identityTime(a.ExpiresAt)
	if err != nil || !expires.After(issued) || expires.Sub(issued) > IdentityValidityWindow {
		return errors.New("contract: identity validity exceeds 60 seconds")
	}
	if a.UserID != nil && !identityID(string(*a.UserID), 64) {
		return errors.New("contract: identity attribution is invalid")
	}
	return a.Principal.Validate()
}

func identityDecode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("contract: identity has trailing JSON")
	}
	return nil
}

func identityID(value string, maximum int) bool { return value != "" && utf16Length(value) <= maximum }

var identityTimePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?Z$`)

func identityTime(value Timestamp) (time.Time, error) {
	if !identityTimePattern.MatchString(string(value)) {
		return time.Time{}, errors.New("contract: identity timestamp must be UTC")
	}
	if strings.Count(string(value), ":") == 1 {
		return time.Parse("2006-01-02T15:04Z", string(value))
	}
	return time.Parse(time.RFC3339Nano, string(value))
}
