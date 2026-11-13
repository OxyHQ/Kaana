package nativeadmission

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Model is a projection of Kaana's validated declared catalogue, not a second
// catalogue. Deployment eligibility remains the existing executor's decision.
type Model struct {
	ID                                                                contract.ModelID
	PowerClass                                                        string
	Audience                                                          contract.CatalogueAudience
	Active                                                            bool
	InputModalities, OutputModalities                                 []string
	Tools, StructuredOutput, JSONMode, Reasoning, Streaming           bool
	MaxContextTokens, MaxOutputTokens                                 int
	LicenseID                                                         string
	CommercialUseAllowed                                              bool
	RetainsPayloads, TrainsOnCustomerData, ZeroDataRetentionAvailable bool
	MaxInputTokensPerImage                                            int
	Price                                                             ModelPrice
}
type Request struct {
	Operation                             contract.GrantOperation
	Target                                *contract.GrantTarget
	Facts                                 contract.GrantRequestFacts
	StructuredOutput, JSONMode, Reasoning bool
	RequestedEffort                       *contract.ReasoningEffort
}
type Plan struct {
	PolicyID             string                          `json:"policyId"`
	PolicyVersion        int                             `json:"policyVersion"`
	PolicyDigest         string                          `json:"policyDigest"`
	Steps                []contract.GrantStep            `json:"steps"`
	RoutingPolicy        contract.RoutingPolicyReference `json:"routingPolicy"`
	Prices               []ModelPrice                    `json:"prices"`
	AllowedDeploymentIDs []contract.DeploymentID         `json:"allowedDeploymentIds"`
	MaxConcurrent        int                             `json:"maxConcurrent"`
	MaxPerUTCDay         int                             `json:"maxPerUtcDay"`
	ExecutionTimeout     time.Duration                   `json:"executionTimeout"`
	InputTokens          int                             `json:"inputTokens"`
	OutputTokens         int                             `json:"outputTokens"`
	Quote                contract.Money                  `json:"quote"`
}
type Planner struct {
	policies []Policy
	models   []Model
}

// NewPlanner snapshots policy and catalogue. No built-in application or model
// enrollments exist. Misconfiguration fails before the server admits traffic.
func NewPlanner(policies []Policy, catalogue []Model) (*Planner, error) {
	seen := map[string]bool{}
	for _, policy := range policies {
		if err := policy.validate(); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%t", policy.OwnerAccountID, policy.ApplicationID, policy.Environment, policy.Lane, policy.Authority, policy.ApplicationClass, policy.ApplicationIsInternal)
		if seen[key] {
			return nil, errors.New("nativeadmission: ambiguous policy enrollment")
		}
		seen[key] = true
	}
	modelIDs := map[contract.ModelID]bool{}
	versions := map[string]ModelPrice{}
	for _, model := range catalogue {
		if !model.ID.Valid() || modelIDs[model.ID] {
			return nil, errors.New("nativeadmission: duplicate or invalid model")
		}
		modelIDs[model.ID] = true
		if model.Price.PriceVersionID != "" {
			if model.Price.ModelLine != model.ID {
				return nil, ErrUnpriced
			}
			if _, err := unitRates(model.Price); err != nil {
				return nil, err
			}
			// A version is immutable across the complete snapshot, not merely per unit.
			if old, ok := versions[model.Price.PriceVersionID]; ok && !samePrice(old, model.Price) {
				return nil, errors.New("nativeadmission: conflicting price version")
			}
			versions[model.Price.PriceVersionID] = model.Price
		}
	}
	return &Planner{policies: clone(policies), models: clone(catalogue)}, nil
}
func samePrice(a, b ModelPrice) bool {
	if a.ModelLine != b.ModelLine || a.Currency != b.Currency || len(a.UnitPrices) != len(b.UnitPrices) {
		return false
	}
	ar, _ := unitRates(a)
	br, _ := unitRates(b)
	for unit, rate := range ar {
		other, ok := br[unit]
		if !ok || rate.Cmp(other) != 0 {
			return false
		}
	}
	return true
}

// AutoFloor preserves the capability/size floors of the existing auto policy.
// It examines content-free controls only; no inference call or prompt inspection.
func AutoFloor(request Request) contract.PowerLevel {
	rank := 0
	raise := func(n int) {
		if n > rank {
			rank = n
		}
	}
	if request.Facts.DeclaredTools > 0 || request.StructuredOutput || request.JSONMode || request.Reasoning || value(request.Facts.InputImages) > 0 {
		raise(1)
	}
	if request.Facts.DeclaredTools > 8 {
		raise(2)
	}
	if value(request.Facts.EstimatedInputTokens) > 8000 || value(request.Facts.MaxOutputTokens) > 4096 {
		raise(1)
	}
	if value(request.Facts.EstimatedInputTokens) > 64000 || value(request.Facts.MaxOutputTokens) > 16000 {
		raise(2)
	}
	if request.RequestedEffort != nil {
		switch *request.RequestedEffort {
		case contract.ReasoningEffortLow:
			raise(1)
		case contract.ReasoningEffortMedium:
			raise(2)
		case contract.ReasoningEffortHigh:
			raise(3)
		}
	}
	return []contract.PowerLevel{contract.PowerInstant, contract.PowerMedium, contract.PowerHigh, contract.PowerXHigh}[rank]
}
func value(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
func class(level contract.PowerLevel) string {
	if level == contract.PowerXHigh {
		return "high"
	}
	return string(level)
}

func (p *Planner) Plan(identity contract.IdentityPrincipal, request Request) (Plan, error) {
	if err := identity.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: invalid identity", ErrNotEnrolled)
	}
	var policy *Policy
	for i := range p.policies {
		if p.policies[i].matches(identity) {
			policy = &p.policies[i]
			break
		}
	}
	if policy == nil {
		return Plan{}, ErrNotEnrolled
	}
	if identity.Authority == contract.AuthorityExternal && !slices.Contains(identity.Scopes, contract.ScopeInvoke) {
		return Plan{}, ErrNotEnrolled
	}
	if request.Operation != contract.GrantResponses && request.Operation != contract.GrantChatCompletions {
		return Plan{}, fmt.Errorf("%w: operation is not yet supported", ErrInvalidRequest)
	}
	if request.Target == nil || request.Target.Validate() != nil || request.Facts.EstimatedInputTokens == nil || value(request.Facts.EstimatedInputTokens) < 0 || value(request.Facts.EstimatedInputTokens) > policy.MaxInputTokens || request.Facts.DeclaredTools < 0 || request.Facts.DeclaredTools > policy.MaxTools || value(request.Facts.InputImages) < 0 {
		return Plan{}, ErrInvalidRequest
	}
	if request.Facts.InputCharacters != nil || request.Facts.AudioInputMilliseconds != nil || request.Facts.EmbeddingInputs != nil || request.Facts.Images != nil || request.Facts.RealtimeSessionKind != nil {
		return Plan{}, ErrInvalidRequest
	}
	if request.RequestedEffort != nil && *request.RequestedEffort != contract.ReasoningEffortLow && *request.RequestedEffort != contract.ReasoningEffortMedium && *request.RequestedEffort != contract.ReasoningEffortHigh {
		return Plan{}, ErrInvalidRequest
	}
	output := policy.DefaultOutputTokens
	if request.Facts.MaxOutputTokens != nil {
		output = *request.Facts.MaxOutputTokens
	}
	if output <= 0 || output > policy.MaxOutputTokens {
		return Plan{}, fmt.Errorf("%w: output exceeds policy", ErrInvalidRequest)
	}
	// Do not silently shrink a declared output allowance. It is both a reasoning
	// budget and an economic request; a smaller value changes the caller's task.
	normalized := request
	normalized.Facts.MaxOutputTokens = &output
	targets := []contract.GrantTarget{}
	if request.Target.Kind == contract.GrantTargetModel {
		if !slices.Contains(policy.AllowedModels, *request.Target.ModelLine) {
			return Plan{}, ErrNoRoute
		}
		targets = append(targets, *request.Target)
	} else if *request.Target.Level == contract.PowerAuto {
		if !containsLevel(*policy, contract.PowerAuto) {
			return Plan{}, ErrNoRoute
		}
		ladder := []contract.PowerLevel{contract.PowerInstant, contract.PowerMedium, contract.PowerHigh, contract.PowerXHigh}
		start := slices.Index(ladder, AutoFloor(normalized))
		for _, level := range ladder[start:] {
			if containsLevel(*policy, level) {
				targets = append(targets, contract.LevelTarget(level))
			}
		}
	} else if containsLevel(*policy, *request.Target.Level) {
		targets = append(targets, *request.Target)
	}
	plan := Plan{PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyDigest: policyDigest(*policy), RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: policy.ID, PolicyVersion: policy.Version}, AllowedDeploymentIDs: clone(policy.AllowedDeploymentIDs), MaxConcurrent: policy.MaxConcurrent, MaxPerUTCDay: policy.MaxPerUTCDay, ExecutionTimeout: policy.ExecutionTimeout, InputTokens: value(request.Facts.EstimatedInputTokens), OutputTokens: output}
	ceiling, _ := moneyRat(*policy.Limits.MaxPricePerRequest)
	maxQuote := new(big.Rat)
	seen := map[contract.ModelID]bool{}
	for _, target := range targets {
		type candidate struct {
			model Model
			quote *big.Rat
			input int
		}
		var candidates []candidate
		for _, model := range p.models {
			if seen[model.ID] || !eligible(*policy, model, normalized) {
				continue
			}
			if target.Kind == contract.GrantTargetModel && model.ID != *target.ModelLine {
				continue
			}
			if target.Kind == contract.GrantTargetLevel && model.PowerClass != class(*target.Level) {
				continue
			}
			input := value(request.Facts.EstimatedInputTokens)
			images := value(request.Facts.InputImages)
			if images > 0 {
				if model.MaxInputTokensPerImage <= 0 || images > (policy.MaxInputTokens-input)/model.MaxInputTokensPerImage {
					continue
				}
				input += images * model.MaxInputTokensPerImage
			}
			if input > policy.MaxInputTokens || model.MaxContextTokens <= 0 || model.MaxOutputTokens <= 0 || output > model.MaxOutputTokens || input > model.MaxContextTokens-output {
				continue
			}
			quoted, err := textQuote(model.Price, input, output)
			if err != nil {
				continue
			}
			quote, _ := moneyRat(quoted)
			if quote.Cmp(ceiling) > 0 {
				continue
			}
			candidates = append(candidates, candidate{model, quote, input})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if cmp := candidates[i].quote.Cmp(candidates[j].quote); cmp != 0 {
				return cmp < 0
			}
			return candidates[i].model.ID < candidates[j].model.ID
		})
		limits := clone(policy.Limits)
		for _, c := range candidates {
			seen[c.model.ID] = true
			limits.AdmittedModelLines = append(limits.AdmittedModelLines, contract.AdmittedModelLine{ModelLine: c.model.ID, PriceVersionID: c.model.Price.PriceVersionID})
			plan.Prices = append(plan.Prices, clone(c.model.Price))
			if c.quote.Cmp(maxQuote) > 0 {
				maxQuote.Set(c.quote)
			}
			if c.input > plan.InputTokens {
				plan.InputTokens = c.input
			}
		}
		if len(limits.AdmittedModelLines) > 0 {
			plan.Steps = append(plan.Steps, contract.GrantStep{Target: target, Limits: limits})
		}
	}
	if len(plan.Steps) == 0 {
		return Plan{}, ErrNoRoute
	}
	plan.Quote = roundedMoney(maxQuote)
	return plan, nil
}

func eligible(policy Policy, model Model, r Request) bool {
	if !model.Active || !admittedAudience(policy, model.Audience) || !slices.Contains(model.InputModalities, "text") || !slices.Contains(model.OutputModalities, "text") {
		return false
	}
	if r.Facts.DeclaredTools > 0 && !model.Tools || r.StructuredOutput && !model.StructuredOutput || r.JSONMode && !model.JSONMode || (r.Reasoning || r.RequestedEffort != nil) && !model.Reasoning || r.Facts.Stream && !model.Streaming {
		return false
	}
	if value(r.Facts.InputImages) > 0 && !slices.Contains(model.InputModalities, "image") {
		return false
	}
	if policy.Limits.RequireZeroDataRetention && (model.RetainsPayloads || !model.ZeroDataRetentionAvailable) {
		return false
	}
	if policy.Limits.ProhibitTrainingOnCustomerData && model.TrainsOnCustomerData {
		return false
	}
	if policy.Limits.RequireCommercialUseRights && !model.CommercialUseAllowed {
		return false
	}
	return len(policy.Limits.AllowedLicenseIDs) == 0 || slices.Contains(policy.Limits.AllowedLicenseIDs, model.LicenseID)
}
