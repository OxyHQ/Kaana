package nativeadmission

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Model is a projection of Kaana's validated declared catalogue, not a second
// catalogue. Deployment eligibility remains the existing executor's decision.
type Model struct {
	// Unreported fields are unknown, not authoritative false. A structured
	// request may use them only with affirmative current parameter evidence.
	Unreported                                                        []string
	AcceptedParameters                                                []provider.RequestParameter
	APIFormats                                                        []string
	RealtimeSessionKinds                                              []contract.RealtimeSessionKind
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
	SessionMaxDuration                    time.Duration
	SessionMaxResponses                   int
	Operation                             contract.GrantOperation
	Target                                *contract.GrantTarget
	Facts                                 contract.GrantRequestFacts
	StructuredOutput, JSONMode, Reasoning bool
	RequestedEffort                       *contract.ReasoningEffort
}
type Plan struct {
	Operation           contract.GrantOperation                          `json:"operation"`
	SessionMaxDuration  time.Duration                                    `json:"sessionMaxDuration,omitempty"`
	SessionMaxResponses int                                              `json:"sessionMaxResponses,omitempty"`
	SessionKind         *contract.RealtimeSessionKind                    `json:"sessionKind,omitempty"`
	RequiredParameters  map[contract.ModelID][]provider.RequestParameter `json:"requiredParameters,omitempty"`
	PolicyID            string                                           `json:"policyId"`
	PolicyVersion       int                                              `json:"policyVersion"`
	PolicyDigest        string                                           `json:"policyDigest"`
	Steps               []contract.GrantStep                             `json:"steps"`
	RoutingPolicy       contract.RoutingPolicyReference                  `json:"routingPolicy"`
	Prices              []ModelPrice                                     `json:"prices"`
	StepRoutes          []RouteScope                                     `json:"stepRoutes"`
	MaxConcurrent       int                                              `json:"maxConcurrent"`
	MaxPerUTCDay        int                                              `json:"maxPerUtcDay"`
	ExecutionTimeout    time.Duration                                    `json:"executionTimeout"`
	InputTokens         int                                              `json:"inputTokens"`
	OutputTokens        int                                              `json:"outputTokens"`
	Quote               contract.Money                                   `json:"quote"`
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
	if identity.Authority == contract.IdentityAuthorityExternal && !slices.Contains(identity.Scopes, contract.ScopeInvoke) {
		return Plan{}, ErrNotEnrolled
	}
	if request.Operation != contract.GrantResponses && request.Operation != contract.GrantChatCompletions {
		return p.planNonText(identity, *policy, request)
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
	if request.SessionMaxDuration != 0 || request.SessionMaxResponses != 0 {
		return Plan{}, ErrInvalidRequest
	}
	if value(request.Facts.InputImages) > 0 && (policy.ImageInput == nil || value(request.Facts.InputImages) > policy.ImageInput.MaxImages) {
		return Plan{}, ErrInvalidRequest
	}
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
		if policy.Promotional != nil && slices.Contains(policy.Promotional.Levels, *request.Target.Level) && containsLevel(*policy, policy.Promotional.FallbackLevel) {
			targets = append(targets, contract.LevelTarget(policy.Promotional.FallbackLevel))
		}
	}
	plan := Plan{Operation: request.Operation, PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyDigest: policyDigest(*policy), RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: policy.ID, PolicyVersion: policy.Version}, MaxConcurrent: policy.MaxConcurrent, MaxPerUTCDay: policy.MaxPerUTCDay, ExecutionTimeout: policy.ExecutionTimeout, InputTokens: value(request.Facts.EstimatedInputTokens), OutputTokens: output}
	maxQuote := new(big.Rat)
	seen := map[contract.ModelID]bool{}
	for _, target := range targets {
		effective := *policy
		routes := policy.Routes
		if value(request.Facts.InputImages) > 0 {
			effective.Limits = policy.ImageInput.Limits
			routes = policy.ImageInput.Routes
		}
		if policy.Promotional != nil && target.Kind == contract.GrantTargetLevel && request.Target.Kind == contract.GrantTargetLevel && *target.Level == *request.Target.Level && slices.Contains(policy.Promotional.Levels, *target.Level) {
			effective.Limits = policy.Promotional.Limits
			routes = policy.Promotional.Routes
		}
		ceiling, _ := moneyRat(*effective.Limits.MaxPricePerRequest)
		type candidate struct {
			model Model
			quote *big.Rat
			input int
		}
		var candidates []candidate
		for _, model := range p.models {
			if seen[model.ID] || !eligible(effective, model, normalized) {
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
				if model.MaxInputTokensPerImage <= 0 || model.MaxContextTokens-output-input < 0 || images > (model.MaxContextTokens-output-input)/model.MaxInputTokensPerImage {
					continue
				}
				input += images * model.MaxInputTokensPerImage
			}
			if model.MaxContextTokens <= 0 || model.MaxOutputTokens <= 0 || output > model.MaxOutputTokens || input > model.MaxContextTokens-output {
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
		limits := clone(effective.Limits)
		for _, c := range candidates {
			seen[c.model.ID] = true
			limits.AdmittedModelLines = append(limits.AdmittedModelLines, contract.AdmittedModelLine{ModelLine: c.model.ID, PriceVersionID: c.model.Price.PriceVersionID})
			plan.Prices = append(plan.Prices, clone(c.model.Price))
			if required := structuredRequirements(c.model, normalized); len(required) > 0 {
				if plan.RequiredParameters == nil {
					plan.RequiredParameters = make(map[contract.ModelID][]provider.RequestParameter)
				}
				plan.RequiredParameters[c.model.ID] = required
			}
			if c.quote.Cmp(maxQuote) > 0 {
				maxQuote.Set(c.quote)
			}
			if c.input > plan.InputTokens {
				plan.InputTokens = c.input
			}
		}
		if len(limits.AdmittedModelLines) > 0 {
			plan.Steps = append(plan.Steps, contract.GrantStep{Target: target, Limits: limits})
			plan.StepRoutes = append(plan.StepRoutes, clone(routes))
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
	if r.Facts.DeclaredTools > 0 && !model.Tools || !structuredAllowed(model, "structuredOutput", model.StructuredOutput, r.StructuredOutput) || !structuredAllowed(model, "jsonMode", model.JSONMode, r.JSONMode) || (r.Reasoning || r.RequestedEffort != nil) && !model.Reasoning || r.Facts.Stream && !model.Streaming {
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

func (p *Planner) planNonText(identity contract.IdentityPrincipal, policy Policy, r Request) (Plan, error) {
	var terms *OperationPolicy
	switch r.Operation {
	case contract.GrantAudioSpeech:
		terms = policy.Speech
	case contract.GrantAudioTranscriptions:
		terms = policy.Transcription
	case contract.GrantRealtimeSession:
		terms = policy.Realtime
	default:
		return Plan{}, ErrInvalidRequest
	}
	if terms == nil || r.Target == nil || r.Target.Validate() != nil || r.Target.Kind != contract.GrantTargetModel {
		return Plan{}, ErrInvalidRequest
	}
	if !terms.AllowCatalogueModels && !slices.Contains(policy.AllowedModels, *r.Target.ModelLine) {
		return Plan{}, ErrNoRoute
	}
	if r.StructuredOutput || r.JSONMode || r.Reasoning || r.RequestedEffort != nil || r.Facts.MaxOutputTokens != nil || r.Facts.EmbeddingInputs != nil || r.Facts.Images != nil || r.Facts.InputImages != nil {
		return Plan{}, ErrInvalidRequest
	}
	if r.Facts.DeclaredTools < 0 || r.Facts.DeclaredTools > policy.MaxTools {
		return Plan{}, ErrInvalidRequest
	}
	input := 0
	switch r.Operation {
	case contract.GrantAudioSpeech:
		if r.Facts.InputCharacters == nil || r.Facts.AudioInputMilliseconds != nil || r.Facts.RealtimeSessionKind != nil || r.Facts.Stream || r.Facts.DeclaredTools != 0 || r.Facts.EstimatedInputTokens != nil {
			return Plan{}, ErrInvalidRequest
		}
		input = *r.Facts.InputCharacters
	case contract.GrantAudioTranscriptions:
		if r.Facts.AudioInputMilliseconds == nil || r.Facts.InputCharacters != nil || r.Facts.RealtimeSessionKind != nil || r.Facts.DeclaredTools != 0 || r.Facts.EstimatedInputTokens != nil {
			return Plan{}, ErrInvalidRequest
		}
		input = *r.Facts.AudioInputMilliseconds
	case contract.GrantRealtimeSession:
		if r.Facts.RealtimeSessionKind == nil || !slices.Contains(terms.SessionKinds, *r.Facts.RealtimeSessionKind) || r.Facts.InputCharacters != nil || r.Facts.AudioInputMilliseconds != nil || r.SessionMaxDuration < time.Millisecond || r.SessionMaxDuration%time.Millisecond != 0 || r.SessionMaxDuration > terms.MaxSessionDuration || r.SessionMaxResponses <= 0 || r.SessionMaxResponses > terms.MaxSessionResponses {
			return Plan{}, ErrInvalidRequest
		}
		input = value(r.Facts.EstimatedInputTokens)
	}
	if input < 0 || input > terms.MaxInput || ((r.Operation == contract.GrantAudioSpeech || r.Operation == contract.GrantAudioTranscriptions) && input == 0) {
		return Plan{}, ErrInvalidRequest
	}
	if r.Operation != contract.GrantRealtimeSession && (r.SessionMaxDuration != 0 || r.SessionMaxResponses != 0) {
		return Plan{}, ErrInvalidRequest
	}
	ceiling, _ := moneyRat(*terms.Limits.MaxPricePerRequest)
	for _, m := range p.models {
		if m.ID != *r.Target.ModelLine || !m.Active || !admittedAudience(policy, m.Audience) || !privacyEligible(terms.Limits, m) {
			continue
		}
		switch r.Operation {
		case contract.GrantAudioSpeech:
			if !slices.Contains(m.APIFormats, "audio_speech") || !slices.Contains(m.InputModalities, "text") || !slices.Contains(m.OutputModalities, "audio") {
				continue
			}
		case contract.GrantAudioTranscriptions:
			if !slices.Contains(m.APIFormats, "audio_transcriptions") || !slices.Contains(m.InputModalities, "audio") || !slices.Contains(m.OutputModalities, "text") {
				continue
			}
		case contract.GrantRealtimeSession:
			if !slices.Contains(m.RealtimeSessionKinds, *r.Facts.RealtimeSessionKind) || !slices.Contains(m.InputModalities, "audio") || r.Facts.DeclaredTools > 0 && !m.Tools || m.MaxContextTokens <= 0 || input > m.MaxContextTokens {
				continue
			}
			output := "audio"
			if *r.Facts.RealtimeSessionKind == "transcription" {
				output = "text"
			}
			if !slices.Contains(m.OutputModalities, output) {
				continue
			}
		}
		quote, qualification, err := operationQuote(m.Price, r)
		if err != nil {
			continue
		}
		amount, _ := moneyRat(qualification)
		if amount.Cmp(ceiling) > 0 {
			continue
		}
		limits := clone(terms.Limits)
		limits.AdmittedModelLines = []contract.AdmittedModelLine{{ModelLine: m.ID, PriceVersionID: m.Price.PriceVersionID}}
		timeout := policy.ExecutionTimeout
		if r.Operation == contract.GrantRealtimeSession {
			timeout = r.SessionMaxDuration
		}
		return Plan{Operation: r.Operation, SessionMaxDuration: r.SessionMaxDuration, SessionMaxResponses: r.SessionMaxResponses, SessionKind: r.Facts.RealtimeSessionKind, PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyDigest: policyDigest(policy), Steps: []contract.GrantStep{{Target: *r.Target, Limits: limits}}, StepRoutes: []RouteScope{clone(terms.Routes)}, RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: policy.ID, PolicyVersion: policy.Version}, Prices: []ModelPrice{clone(m.Price)}, MaxConcurrent: policy.MaxConcurrent, MaxPerUTCDay: policy.MaxPerUTCDay, ExecutionTimeout: timeout, InputTokens: value(r.Facts.EstimatedInputTokens), Quote: quote}, nil
	}
	return Plan{}, ErrNoRoute
}

func privacyEligible(limits contract.GrantStepLimits, m Model) bool {
	if limits.RequireZeroDataRetention && (m.RetainsPayloads || !m.ZeroDataRetentionAvailable) || limits.ProhibitTrainingOnCustomerData && m.TrainsOnCustomerData || limits.RequireCommercialUseRights && !m.CommercialUseAllowed {
		return false
	}
	return len(limits.AllowedLicenseIDs) == 0 || slices.Contains(limits.AllowedLicenseIDs, m.LicenseID)
}

func structuredAllowed(model Model, field string, declared, required bool) bool {
	if !required || declared {
		return true
	}
	return slices.Contains(model.Unreported, field) && slices.Contains(model.AcceptedParameters, provider.ParameterResponseFormat)
}
func structuredRequirements(model Model, r Request) []provider.RequestParameter {
	if (r.StructuredOutput && !model.StructuredOutput) || (r.JSONMode && !model.JSONMode) {
		return []provider.RequestParameter{provider.ParameterResponseFormat}
	}
	return nil
}

// AllowsParameters must hold on EACH actual resolved route before admission.
// A different route of an admitted line cannot inherit another route's evidence.
func (p Plan) AllowsParameters(model contract.ModelID, observed *[]provider.RequestParameter) bool {
	required := p.RequiredParameters[model]
	if len(required) == 0 {
		return true
	}
	if observed == nil {
		return false
	}
	for _, parameter := range required {
		if !slices.Contains(*observed, parameter) {
			return false
		}
	}
	return true
}
