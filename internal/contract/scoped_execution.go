package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const ScopedExecutionContractVersion = "3.6.0"
const ScopedRequestEnvelopeVersion = 3

type ScopedExecutionPrincipal struct {
	AccountID     string      `json:"accountId"`
	ApplicationID string      `json:"applicationId"`
	CredentialID  string      `json:"credentialId"`
	Environment   Environment `json:"environment"`
}

type ScopedExecutionAudience struct {
	PermitID                  string                   `json:"permitId"`
	IdempotencyKey            IdempotencyKey           `json:"idempotencyKey"`
	FixtureSHA256             string                   `json:"fixtureSha256"`
	ExpiresAt                 string                   `json:"expiresAt"`
	Principal                 ScopedExecutionPrincipal `json:"principal"`
	Policy                    RoutingPolicyReference   `json:"policy"`
	DeploymentID              DeploymentID             `json:"deploymentId"`
	Provider                  ProviderSlug             `json:"provider"`
	KeyID                     string                   `json:"keyId"`
	ModelReference            ModelReference           `json:"modelReference"`
	UpstreamModelID           string                   `json:"upstreamModelId"`
	PriceVersionID            string                   `json:"priceVersionId"`
	ProviderRateCardVersionID string                   `json:"providerRateCardVersionId"`
	ProviderSourceVersion     string                   `json:"providerSourceVersion"`
	MaxCostUSD                string                   `json:"maxCostUsd"`
}

type ScopedExecution struct {
	ScopedExecutionAudience
	RequestID             RequestID `json:"requestId"`
	SnapshotID            string    `json:"snapshotId"`
	CatalogueEvidenceHash string    `json:"catalogueEvidenceHash"`
}

var scopedDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var scopedCostPattern = regexp.MustCompile(`^0\.[0-9]{1,12}$`)

func (a *ScopedExecutionAudience) Validate() error {
	if a == nil {
		return errors.New("contract: missing scoped audience")
	}
	for _, v := range []string{a.PermitID, string(a.IdempotencyKey), a.Principal.AccountID, a.Principal.ApplicationID, a.Principal.CredentialID, string(a.DeploymentID), a.KeyID, a.UpstreamModelID, a.PriceVersionID, a.ProviderRateCardVersionID, a.ProviderSourceVersion} {
		if utf16Length(v) < 1 || utf16Length(v) > 256 {
			return errors.New("contract: invalid scoped identity")
		}
	}
	if !a.Provider.Valid() || !a.ModelReference.Valid() || !a.ModelReference.Pinned() || (a.Principal.Environment != EnvironmentProduction && a.Principal.Environment != EnvironmentDevelopment) || a.Policy.RoutingPolicyID == "" || utf16Length(a.Policy.RoutingPolicyID) > 128 || a.Policy.PolicyVersion < 1 || a.Policy.PolicyVersion > 9007199254740991 || !scopedDigestPattern.MatchString(a.FixtureSHA256) {
		return errors.New("contract: invalid scoped audience")
	}
	if _, err := time.Parse(time.RFC3339Nano, a.ExpiresAt); err != nil || !strings.HasSuffix(a.ExpiresAt, "Z") {
		return errors.New("contract: invalid scoped expiration")
	}
	if !scopedCostPattern.MatchString(a.MaxCostUSD) {
		return errors.New("contract: invalid scoped cost ceiling")
	}
	fraction := strings.TrimPrefix(a.MaxCostUSD, "0.")
	padded := fraction + strings.Repeat("0", 12-len(fraction))
	if padded == strings.Repeat("0", 12) || padded > "010000000000" {
		return errors.New("contract: invalid scoped cost ceiling")
	}
	return nil
}

func (a *ScopedExecutionAudience) Equal(b *ScopedExecutionAudience) bool {
	return a != nil && b != nil && *a == *b
}
func (a *ScopedExecutionAudience) NotExpired(at time.Time) bool {
	if a == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
	return err == nil && at.Before(expires)
}

var scopedAudienceMembers = map[string]bool{"permitId": true, "idempotencyKey": true, "fixtureSha256": true, "expiresAt": true, "principal": true, "policy": true, "deploymentId": true, "provider": true, "keyId": true, "modelReference": true, "upstreamModelId": true, "priceVersionId": true, "providerRateCardVersionId": true, "providerSourceVersion": true, "maxCostUsd": true}

func validateScopedMembers(raw []byte, extended bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return errors.New("contract: scoped execution must be an object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		allowed := scopedAudienceMembers[key] || (extended && (key == "requestId" || key == "snapshotId" || key == "catalogueEvidenceHash"))
		if !ok || seen[key] || !allowed {
			return errors.New("contract: ambiguous or unknown scoped member")
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		if key == "principal" || key == "policy" {
			allowed := map[string]bool{"accountId": true, "applicationId": true, "credentialId": true, "environment": true}
			if key == "policy" {
				allowed = map[string]bool{"routingPolicyId": true, "policyVersion": true}
			}
			if err = validateScopedLeaf(value, allowed); err != nil {
				return err
			}
		}
	}
	if _, err = d.Token(); err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("contract: trailing scoped JSON")
	}
	return nil
}
func validateScopedLeaf(raw []byte, allowed map[string]bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return errors.New("contract: invalid scoped nested object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] || !allowed[key] {
			return errors.New("contract: ambiguous scoped nested member")
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
func (a *ScopedExecutionAudience) UnmarshalJSON(raw []byte) error {
	if err := validateScopedMembers(raw, false); err != nil {
		return err
	}
	type wire ScopedExecutionAudience
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*a = ScopedExecutionAudience(value)
	return a.Validate()
}
func (s *ScopedExecution) UnmarshalJSON(raw []byte) error {
	if err := validateScopedMembers(raw, true); err != nil {
		return err
	}
	type audienceWire ScopedExecutionAudience
	var value struct {
		audienceWire
		RequestID             RequestID `json:"requestId"`
		SnapshotID            string    `json:"snapshotId"`
		CatalogueEvidenceHash string    `json:"catalogueEvidenceHash"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*s = ScopedExecution{ScopedExecutionAudience: ScopedExecutionAudience(value.audienceWire), RequestID: value.RequestID, SnapshotID: value.SnapshotID, CatalogueEvidenceHash: value.CatalogueEvidenceHash}
	if err := s.ScopedExecutionAudience.Validate(); err != nil {
		return err
	}
	if s.RequestID == "" || s.SnapshotID == "" || !scopedDigestPattern.MatchString(s.CatalogueEvidenceHash) {
		return errors.New("contract: invalid scoped execution binding")
	}
	return nil
}

func (s *ScopedExecution) Validate() error {
	if s == nil {
		return errors.New("contract: missing scoped execution")
	}
	if err := s.ScopedExecutionAudience.Validate(); err != nil {
		return err
	}
	if utf16Length(string(s.RequestID)) < 1 || utf16Length(string(s.RequestID)) > 256 || utf16Length(s.SnapshotID) < 1 || utf16Length(s.SnapshotID) > 256 || !scopedDigestPattern.MatchString(s.CatalogueEvidenceHash) {
		return errors.New("contract: invalid scoped execution binding")
	}
	return nil
}

// ValidateScopedInputBytes hashes the exact signed whole input JSON. It never
// reserializes or normalizes the payload across Go and JavaScript encoders.
func (r *Request) ValidateScopedInputBytes(raw []byte) error {
	if r.ScopedExecution == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return errors.New("contract: invalid scoped envelope")
	}
	allowed := map[string]bool{}
	kind := reflect.TypeFor[Request]()
	for i := 0; i < kind.NumField(); i++ {
		allowed[strings.Split(kind.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || !allowed[key] {
			return errors.New("contract: unknown scoped envelope member")
		}
		if _, seen := fields[key]; seen {
			return errors.New("contract: duplicate scoped envelope member")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		fields[key] = value
	}
	if _, err = d.Token(); err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("contract: trailing scoped envelope")
	}
	input, ok := fields["input"]
	if !ok {
		return errors.New("contract: missing scoped input")
	}
	sum := sha256.Sum256(input)
	if hex.EncodeToString(sum[:]) != r.ScopedExecution.FixtureSHA256 {
		return errors.New("contract: scoped fixture hash mismatch")
	}
	return nil
}
func (r *Request) validateScopedExecution() error {
	s := r.ScopedExecution
	if s == nil {
		if r.SchemaVersion == ScopedRequestEnvelopeVersion {
			return errors.New("contract: scoped envelope requires scoped execution")
		}
		return nil
	}
	if r.SchemaVersion != ScopedRequestEnvelopeVersion {
		return errors.New("contract: scoped execution requires envelope version 3")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	p := r.Attribution.Principal
	if s.RequestID != r.Attribution.RequestID || s.Principal.AccountID != string(p.Billing.AccountID) || s.Principal.ApplicationID != string(p.ApplicationID) || s.Principal.CredentialID != string(p.CredentialID) || s.Principal.Environment != p.Environment || s.Policy != r.RoutingPolicy || r.IdempotencyKey == nil || s.IdempotencyKey != *r.IdempotencyKey || len(r.AuthorizedRoutes) != 1 || r.Input.Format != InputDecisions {
		return errors.New("contract: scoped audience does not match signed request")
	}
	route := r.AuthorizedRoutes[0]
	if route.DeploymentID != s.DeploymentID || route.Provider != s.Provider || route.ModelReference != s.ModelReference || route.CustomerProviderCredential != nil {
		return errors.New("contract: scoped execution requires one exact platform route")
	}
	return nil
}
