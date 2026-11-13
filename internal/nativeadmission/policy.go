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
	"strings"
	"time"
	"unicode/utf16"

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
	EconomicTreatment     EconomicTreatment                 `json:"economicTreatment"`
	CredentialIDs         []contract.CredentialID           `json:"credentialIds"`
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
	Routes                RouteScope                        `json:"routes"`
	ImageInput            *ImageInputPolicy                 `json:"imageInput,omitempty"`
	Promotional           *PromotionalPolicy                `json:"promotional,omitempty"`
	Speech                *OperationPolicy                  `json:"speech,omitempty"`
	Transcription         *OperationPolicy                  `json:"transcription,omitempty"`
	Realtime              *OperationPolicy                  `json:"realtime,omitempty"`
	Limits                contract.GrantStepLimits          `json:"limits"`
	MaxInputTokens        int                               `json:"maxInputTokens"`
	MaxOutputTokens       int                               `json:"maxOutputTokens"`
	DefaultOutputTokens   int                               `json:"defaultOutputTokens"`
	MaxTools              int                               `json:"maxTools"`
	MaxConcurrent         int                               `json:"maxConcurrent"`
	MaxPerUTCDay          int                               `json:"maxPerUtcDay"`
	ExecutionTimeout      time.Duration                     `json:"executionTimeout"`
}

func (p Policy) matches(i contract.IdentityPrincipal) bool { return p.Enabled && p.Selects(i) }

// Selects identifies an explicitly enrolled population, even while its policy is disabled.
// It is classification only, never authorization or permission to execute.
func (p Policy) Selects(i contract.IdentityPrincipal) bool {
	return slices.Contains(p.CredentialIDs, i.CredentialID) && i.OwnerAccountID != nil && p.OwnerAccountID == *i.OwnerAccountID && p.ApplicationID == i.ApplicationID && p.Environment == i.Environment && p.Lane == i.Lane && contract.IdentityAuthority(p.Authority) == i.Authority && p.ApplicationClass == i.ApplicationClass && p.ApplicationIsInternal == i.ApplicationIsInternal
}

func (p Policy) validate() error {
	if !p.EconomicTreatment.valid() {
		return errors.New("nativeadmission: explicit economic treatment required")
	}
	if len(p.CredentialIDs) == 0 {
		return errors.New("nativeadmission: an explicit credential population is required")
	}
	seenCredentials := make(map[contract.CredentialID]bool, len(p.CredentialIDs))
	for _, id := range p.CredentialIDs {
		if id == "" || len(utf16.Encode([]rune(id))) > 64 || seenCredentials[id] {
			return errors.New("nativeadmission: credential population is invalid")
		}
		seenCredentials[id] = true
	}

	identity := contract.IdentityPrincipal{OwnerAccountID: &p.OwnerAccountID, ApplicationID: p.ApplicationID, CredentialID: "policy-validation", Environment: p.Environment, Lane: p.Lane, Authority: contract.IdentityAuthority(p.Authority), ApplicationClass: p.ApplicationClass, ApplicationIsInternal: p.ApplicationIsInternal}
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
	if len(p.AllowedLevels)+len(p.AllowedModels) == 0 {
		return errors.New("nativeadmission: policy requires explicit models or levels and deployments")
	}
	if p.Limits.MaxPricePerRequest == nil || (p.Limits.MaxFundingTier != nil && !p.Limits.MaxFundingTier.Valid()) || len(p.Limits.AdmittedModelLines) != 0 {
		return errors.New("nativeadmission: policy requires price bounds and valid optional funding ceilings and cannot pre-admit models")
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
	if err := validateTerms(p.Limits, p.Routes); err != nil {
		return err
	}
	if p.ImageInput != nil {
		if p.ImageInput.MaxImages <= 0 || p.ImageInput.MaxImages > 128 {
			return errors.New("nativeadmission: invalid image bound")
		}
		if err := validateTerms(p.ImageInput.Limits, p.ImageInput.Routes); err != nil {
			return err
		}
	}
	if p.Promotional != nil {
		promo := p.Promotional
		if len(promo.Levels) == 0 || !promo.FallbackLevel.Valid() || promo.FallbackLevel == contract.PowerAuto || !promo.Routes.RequirePromotionalCredit {
			return errors.New("nativeadmission: invalid promotional policy")
		}
		for _, level := range promo.Levels {
			if level != contract.PowerPro && level != contract.PowerUltra {
				return errors.New("nativeadmission: promotion requires explicit premium level")
			}
		}
		if err := validateTerms(promo.Limits, promo.Routes); err != nil {
			return err
		}
	}
	for _, op := range []*OperationPolicy{p.Speech, p.Transcription, p.Realtime} {
		if op == nil {
			continue
		}
		if op.MaxInput <= 0 {
			return errors.New("nativeadmission: operation input bound required")
		}
		if err := validateTerms(op.Limits, op.Routes); err != nil {
			return err
		}
	}
	if p.Realtime != nil {
		if p.Realtime.MaxSessionDuration <= 0 || p.Realtime.MaxSessionDuration > time.Hour || p.Realtime.MaxSessionResponses <= 0 || len(p.Realtime.SessionKinds) == 0 {
			return errors.New("nativeadmission: realtime bounds required")
		}
		for _, kind := range p.Realtime.SessionKinds {
			if kind != "conversation" && kind != "transcription" && kind != "translation" {
				return errors.New("nativeadmission: invalid realtime kind")
			}
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

// Exact permissions bind the complete reviewed deployment tuple, not a mutable
// deployment ID alone. Eligible scope is an explicit policy choice and still
// requires current commercial/funding/region checks in route resolution.
type RoutePermission struct {
	DeploymentID   contract.DeploymentID   `json:"deploymentId"`
	ModelReference contract.ModelReference `json:"modelReference"`
	Provider       contract.ProviderSlug   `json:"provider"`
}
type RouteScope struct {
	Mode                     string            `json:"mode"`
	Deployments              []RoutePermission `json:"deployments,omitempty"`
	RequirePromotionalCredit bool              `json:"requirePromotionalCredit"`
}
type ImageInputPolicy struct {
	Limits    contract.GrantStepLimits `json:"limits"`
	Routes    RouteScope               `json:"routes"`
	MaxImages int                      `json:"maxImages"`
}
type PromotionalPolicy struct {
	Levels        []contract.PowerLevel    `json:"levels"`
	FallbackLevel contract.PowerLevel      `json:"fallbackLevel"`
	Limits        contract.GrantStepLimits `json:"limits"`
	Routes        RouteScope               `json:"routes"`
}
type OperationPolicy struct {
	Limits               contract.GrantStepLimits       `json:"limits"`
	Routes               RouteScope                     `json:"routes"`
	MaxInput             int                            `json:"maxInput"`
	AllowCatalogueModels bool                           `json:"allowCatalogueModels"`
	SessionKinds         []contract.RealtimeSessionKind `json:"sessionKinds,omitempty"`
	MaxSessionDuration   time.Duration                  `json:"maxSessionDuration,omitempty"`
	MaxSessionResponses  int                            `json:"maxSessionResponses,omitempty"`
}

func validateTerms(limits contract.GrantStepLimits, routes RouteScope) error {
	if limits.MaxPricePerRequest == nil || (limits.MaxFundingTier != nil && !limits.MaxFundingTier.Valid()) || len(limits.AdmittedModelLines) != 0 {
		return errors.New("nativeadmission: terms require explicit price limits and valid optional funding ceilings")
	}
	if _, err := moneyRat(*limits.MaxPricePerRequest); err != nil {
		return err
	}
	switch routes.Mode {
	case "exact":
		if len(routes.Deployments) == 0 {
			return errors.New("nativeadmission: exact scope cannot be empty")
		}
		for _, d := range routes.Deployments {
			if d.DeploymentID == "" || !d.ModelReference.Valid() || !strings.Contains(string(d.ModelReference), "@") || !d.Provider.Valid() {
				return errors.New("nativeadmission: exact deployment tuple invalid")
			}
		}
	case "eligible":
		if len(routes.Deployments) != 0 {
			return errors.New("nativeadmission: eligible scope cannot carry exact permissions")
		}
	default:
		return errors.New("nativeadmission: route scope must be explicit")
	}
	if routes.RequirePromotionalCredit && (limits.MaxFundingTier == nil || *limits.MaxFundingTier != contract.FundingTierCredit) {
		return errors.New("nativeadmission: promotional scope requires credit ceiling")
	}
	return nil
}

// Allows applies this step's extra permissions after the ordinary live
// commercial, provider, region and funding filters. PromotionalEligible must
// come from current available key-grant evidence, never a model's price class.
func (s RouteScope) Allows(id contract.DeploymentID, model contract.ModelReference, provider contract.ProviderSlug, promotionalEligible bool) bool {
	if s.RequirePromotionalCredit && !promotionalEligible {
		return false
	}
	if s.Mode == "eligible" {
		return true
	}
	if s.Mode != "exact" {
		return false
	}
	return slices.ContainsFunc(s.Deployments, func(d RoutePermission) bool {
		return d.DeploymentID == id && d.ModelReference == model && d.Provider == provider
	})
}

// admitsExecution binds the operation-specific lease to the active policy.
func (p Policy) admitsExecution(operation contract.GrantOperation, plan Plan) bool {
	if plan.Operation != operation || plan.EconomicTreatment != p.EconomicTreatment {
		return false
	}
	if operation != contract.GrantRealtimeSession {
		return plan.ExecutionTimeout == p.ExecutionTimeout && plan.SessionMaxDuration == 0 && plan.SessionMaxResponses == 0 && plan.SessionKind == nil
	}
	terms := p.Realtime
	return terms != nil && plan.SessionKind != nil && slices.Contains(terms.SessionKinds, *plan.SessionKind) && plan.SessionMaxDuration >= time.Millisecond && plan.SessionMaxDuration%time.Millisecond == 0 && plan.SessionMaxDuration <= terms.MaxSessionDuration && plan.ExecutionTimeout == plan.SessionMaxDuration && plan.SessionMaxResponses > 0 && plan.SessionMaxResponses <= terms.MaxSessionResponses
}
