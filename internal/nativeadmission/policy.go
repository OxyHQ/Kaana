// Package nativeadmission owns Kaana's explicit economic admission policy.
// An authenticated identity is necessary but never sufficient for execution.
package nativeadmission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

var (
	ErrNotEnrolled    = errors.New("nativeadmission: principal is not enrolled")
	ErrInvalidRequest = errors.New("nativeadmission: unsupported or invalid request")
	ErrNoRoute        = errors.New("nativeadmission: no admitted model")
	ErrUnpriced       = errors.New("nativeadmission: usage has no immutable price")
)

// Policy is reviewed configuration, not derived from identity claims or labels.
// Empty configuration and Enabled=false admit nothing. Deployment permissions
// are mandatory: an empty slice never means every deployment.
type Policy struct {
	ID                    string                            `json:"id"`
	Version               int                               `json:"version"`
	Enabled               bool                              `json:"enabled"`
	OwnerAccountID        contract.AccountID                `json:"ownerAccountId"`
	ApplicationID         contract.ApplicationID            `json:"applicationId"`
	Environment           contract.Environment              `json:"environment"`
	Lane                  contract.IdentityLane             `json:"lane"`
	Authority             contract.ServiceAuthority         `json:"authority"`
	ApplicationClass      contract.IdentityApplicationClass `json:"applicationClass"`
	ApplicationIsInternal bool                              `json:"applicationIsInternal"`
	Audience              contract.CatalogueAudience        `json:"audience"`
	AllowedLevels         []contract.PowerLevel             `json:"allowedLevels"`
	AllowedModels         []contract.ModelID                `json:"allowedModels"`
	AllowedDeploymentIDs  []contract.DeploymentID           `json:"allowedDeploymentIds"`
	Limits                contract.GrantStepLimits          `json:"limits"`
	MaxInputTokens        int                               `json:"maxInputTokens"`
	MaxOutputTokens       int                               `json:"maxOutputTokens"`
	DefaultOutputTokens   int                               `json:"defaultOutputTokens"`
	MaxTools              int                               `json:"maxTools"`
	MaxConcurrent         int                               `json:"maxConcurrent"`
	MaxPerUTCDay          int                               `json:"maxPerUtcDay"`
	ExecutionTimeout      time.Duration                     `json:"executionTimeout"`
}

func (p Policy) matches(i contract.IdentityPrincipal) bool {
	return p.Enabled && p.OwnerAccountID == i.OwnerAccountID && p.ApplicationID == i.ApplicationID && p.Environment == i.Environment && p.Lane == i.Lane && p.Authority == i.Authority && p.ApplicationClass == i.ApplicationClass && p.ApplicationIsInternal == i.ApplicationIsInternal
}

func (p Policy) validate() error {
	identity := contract.IdentityPrincipal{OwnerAccountID: p.OwnerAccountID, ApplicationID: p.ApplicationID, CredentialID: "policy-validation", Environment: p.Environment, Lane: p.Lane, Authority: p.Authority, ApplicationClass: p.ApplicationClass, ApplicationIsInternal: p.ApplicationIsInternal}
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("nativeadmission: invalid policy principal: %w", err)
	}
	if p.ID == "" || p.Version <= 0 || p.MaxInputTokens <= 0 || p.MaxOutputTokens <= 0 || p.DefaultOutputTokens <= 0 || p.DefaultOutputTokens > p.MaxOutputTokens || p.MaxTools < 0 || p.MaxTools > 128 || p.MaxConcurrent <= 0 || p.MaxPerUTCDay <= 0 || p.ExecutionTimeout <= 0 || p.ExecutionTimeout > 15*time.Minute {
		return errors.New("nativeadmission: policy requires bounded positive limits")
	}
	if p.Audience != contract.AudiencePublic && p.Audience != contract.AudiencePlatformInternal {
		return errors.New("nativeadmission: invalid catalogue audience")
	}
	if p.Audience == contract.AudiencePlatformInternal && (!p.ApplicationIsInternal || p.Authority != contract.AuthorityFirstParty) {
		return errors.New("nativeadmission: internal catalogue requires an enrolled internal workload")
	}
	if len(p.AllowedDeploymentIDs) == 0 || len(p.AllowedLevels)+len(p.AllowedModels) == 0 {
		return errors.New("nativeadmission: policy requires explicit models or levels and deployments")
	}
	if p.Limits.MaxPricePerRequest == nil || p.Limits.MaxFundingTier == nil || !p.Limits.MaxFundingTier.Valid() || len(p.Limits.AdmittedModelLines) != 0 {
		return errors.New("nativeadmission: policy requires price/funding bounds and cannot pre-admit models")
	}
	if _, err := moneyRat(*p.Limits.MaxPricePerRequest); err != nil {
		return err
	}
	for _, level := range p.AllowedLevels {
		if !level.Valid() {
			return errors.New("nativeadmission: invalid policy level")
		}
	}
	for _, model := range p.AllowedModels {
		if !model.Valid() {
			return errors.New("nativeadmission: invalid policy model")
		}
	}
	for _, id := range p.AllowedDeploymentIDs {
		if id == "" {
			return errors.New("nativeadmission: empty deployment permission")
		}
	}
	return nil
}

func policyDigest(p Policy) string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// clone keeps caller mutations from widening an already-reviewed snapshot.
func clone[T any](value T) T {
	raw, _ := json.Marshal(value)
	var copied T
	_ = json.Unmarshal(raw, &copied)
	return copied
}

func admittedAudience(policy Policy, audience contract.CatalogueAudience) bool {
	return audience == contract.AudiencePublic || (policy.Audience == contract.AudiencePlatformInternal && audience == contract.AudiencePlatformInternal)
}

func containsLevel(policy Policy, level contract.PowerLevel) bool {
	return slices.Contains(policy.AllowedLevels, level)
}
