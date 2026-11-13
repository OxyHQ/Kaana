package nativeadmission

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func ptr[T any](v T) *T { return &v }
func principal() contract.IdentityPrincipal {
	return contract.IdentityPrincipal{OwnerAccountID: ptr(contract.AccountID("owner")), ApplicationID: "app", CredentialID: "credential", Environment: contract.EnvironmentProduction, Lane: contract.IdentityLaneService, Authority: contract.IdentityAuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}
}
func policy() Policy {
	i := principal()
	return Policy{EconomicTreatment: InternalMetered, CredentialIDs: []contract.CredentialID{i.CredentialID}, ID: "native-test", Version: 1, Enabled: true, OwnerAccountID: *i.OwnerAccountID, ApplicationID: i.ApplicationID, Environment: i.Environment, Lane: i.Lane, Authority: contract.ServiceAuthority(i.Authority), ApplicationClass: i.ApplicationClass, ApplicationIsInternal: i.ApplicationIsInternal, Audience: contract.AudiencePlatformInternal, AllowedLevels: []contract.PowerLevel{contract.PowerAuto, contract.PowerInstant, contract.PowerMedium, contract.PowerHigh, contract.PowerXHigh, contract.PowerPro}, AllowedModels: []contract.ModelID{"fixture/medium"}, Routes: RouteScope{Mode: "exact", Deployments: []RoutePermission{{DeploymentID: "deployment-reviewed", ModelReference: "fixture/medium@immutable", Provider: "fixture"}}}, Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: "0.05", Currency: "USD"}, MaxFundingTier: ptr(contract.FundingTierCredit), RequireZeroDataRetention: true, ProhibitTrainingOnCustomerData: true}, MaxInputTokens: 126976, MaxOutputTokens: 8192, DefaultOutputTokens: 2048, MaxTools: 128, MaxConcurrent: 32, MaxPerUTCDay: 5000, ExecutionTimeout: time.Minute}
}
func price(id contract.ModelID) ModelPrice {
	return ModelPrice{ModelLine: id, PriceVersionID: string(id) + "-v1", Currency: "USD", UnitPrices: []UnitPrice{{contract.UnitInputTokens, "1", 1000000, "USD"}, {contract.UnitCachedInputTokens, "0.1", 1000000, "USD"}, {contract.UnitCacheWriteInputTokens, "1.25", 1000000, "USD"}, {contract.UnitOutputTokens, "2", 1000000, "USD"}, {contract.UnitReasoningTokens, "2", 1000000, "USD"}, {contract.UnitRequests, "0", 1, "USD"}}}
}
func model(level string) Model {
	id := contract.ModelID("fixture/" + level)
	return Model{ID: id, PowerClass: level, Audience: contract.AudiencePlatformInternal, Active: true, InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}, Tools: true, StructuredOutput: true, JSONMode: true, Reasoning: true, Streaming: true, MaxContextTokens: 131072, MaxOutputTokens: 16384, LicenseID: "reviewed", CommercialUseAllowed: true, ZeroDataRetentionAvailable: true, MaxInputTokensPerImage: 1000, Price: price(id)}
}
func request() Request {
	target := contract.LevelTarget(contract.PowerAuto)
	return Request{Operation: contract.GrantResponses, Target: &target, Facts: contract.GrantRequestFacts{EstimatedInputTokens: ptr(500), MaxOutputTokens: ptr(8192), Stream: true}, StructuredOutput: true}
}
func planner(t *testing.T, p Policy, models ...Model) *Planner {
	t.Helper()
	v, err := NewPlanner([]Policy{p}, models)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIdentityIsNotEconomicEnrollment(t *testing.T) {
	r := request()
	i := principal()
	p := policy()
	m := model("medium")
	empty, err := NewPlanner(nil, []Model{m})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = empty.Plan(i, r); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	p.Enabled = false
	if _, err = planner(t, p, m).Plan(i, r); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	p.Enabled = true
	for _, mutate := range []func(*contract.IdentityPrincipal){func(i *contract.IdentityPrincipal) { i.OwnerAccountID = ptr(contract.AccountID("another")) }, func(i *contract.IdentityPrincipal) { i.ApplicationID = "gemies-brand-is-not-identity" }, func(i *contract.IdentityPrincipal) { i.Environment = contract.EnvironmentStaging }, func(i *contract.IdentityPrincipal) { i.ApplicationIsInternal = false }, func(i *contract.IdentityPrincipal) { i.ApplicationClass = "first_party" }, func(i *contract.IdentityPrincipal) {
		i.Lane = contract.IdentityLaneMachine
		i.Authority = contract.IdentityAuthorityExternal
		i.Scopes = []contract.Scope{contract.ScopeInvoke}
	}} {
		changed := i
		mutate(&changed)
		if _, err = planner(t, p, m).Plan(changed, r); !errors.Is(err, ErrNotEnrolled) {
			t.Fatalf("mismatched identity admitted: %v", err)
		}
	}
	if _, err = planner(t, p, m).Plan(i, r); err != nil {
		t.Fatal(err)
	}
}
func TestAutoCapabilityFloorsAndExplicitPremium(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Request)
		want   contract.PowerLevel
	}{
		{"simple", func(*Request) {}, contract.PowerInstant},
		{"tools", func(r *Request) { r.Facts.DeclaredTools = 1 }, contract.PowerMedium},
		{"many-tools", func(r *Request) { r.Facts.DeclaredTools = 9 }, contract.PowerHigh},
		{"schema", func(r *Request) { r.StructuredOutput = true }, contract.PowerMedium},
		{"json", func(r *Request) { r.JSONMode = true }, contract.PowerMedium},
		{"image", func(r *Request) { r.Facts.InputImages = ptr(1) }, contract.PowerMedium},
		{"long", func(r *Request) { r.Facts.EstimatedInputTokens = ptr(64001) }, contract.PowerHigh},
		{"medium-boundary", func(r *Request) { r.Facts.EstimatedInputTokens = ptr(8000) }, contract.PowerInstant},
		{"medium", func(r *Request) { r.Facts.EstimatedInputTokens = ptr(8001) }, contract.PowerMedium},
		{"reasoning", func(r *Request) { r.RequestedEffort = ptr(contract.ReasoningEffortHigh) }, contract.PowerXHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			r.StructuredOutput = false
			r.Facts.MaxOutputTokens = ptr(512)
			tc.mutate(&r)
			if got := AutoFloor(r); got != tc.want {
				t.Fatalf("%s != %s", got, tc.want)
			}
		})
	}
	p := planner(t, policy(), model("instant"), model("medium"), model("high"), model("pro"))
	plan, err := p.Plan(principal(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || *plan.Steps[0].Target.Level != contract.PowerMedium || *plan.Steps[1].Target.Level != contract.PowerHigh {
		t.Fatalf("unexpected auto ladder: %+v", plan.Steps)
	}
	for _, step := range plan.Steps {
		for _, m := range step.Limits.AdmittedModelLines {
			if m.ModelLine == "fixture/pro" {
				t.Fatal("auto admitted premium")
			}
		}
	}
	r := request()
	r.Target = ptr(contract.LevelTarget(contract.PowerPro))
	if _, err = p.Plan(principal(), r); err != nil {
		t.Fatal(err)
	}
}
func TestExactPinAndBoundedImmutableSnapshot(t *testing.T) {
	original := policy()
	m := model("medium")
	p := planner(t, original, m)
	original.Routes.Deployments[0].DeploymentID = "unreviewed"
	m.Price.UnitPrices[0].Amount = "0"
	r := request()
	r.Target = ptr(contract.ModelTarget("fixture/medium@immutable"))
	plan, err := p.Plan(principal(), r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Target.Reference() != "fixture/medium@immutable" || len(plan.Prices) != 1 || plan.Prices[0].UnitPrices[0].Amount != "1" || plan.StepRoutes[0].Deployments[0].DeploymentID != "deployment-reviewed" {
		t.Fatalf("snapshot changed: %+v", plan)
	}
	if plan.OutputTokens != 8192 || plan.MaxConcurrent != 32 || plan.MaxPerUTCDay != 5000 || plan.PolicyDigest == "" {
		t.Fatal("lost limits")
	}
	plan.Steps[0].Limits.MaxFundingTier = ptr(contract.FundingTierPaid)
	plan.Prices[0].UnitPrices[0].Amount = "0"
	again, err := p.Plan(principal(), r)
	if err != nil || *again.Steps[0].Limits.MaxFundingTier != contract.FundingTierCredit || again.Prices[0].UnitPrices[0].Amount != "1" {
		t.Fatal("returned plan widened planner")
	}
	r.Target = ptr(contract.ModelTarget("fixture/unlisted"))
	if _, err = p.Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
}
func TestCostContextPrivacyAndCapabilitiesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Model)
	}{
		{"unpriced", func(m *Model) { m.Price = ModelPrice{} }},
		{"expensive", func(m *Model) { m.Price.UnitPrices[3].Amount = "100" }},
		{"context", func(m *Model) { m.MaxContextTokens = 8192 }},
		{"unknown-window", func(m *Model) { m.MaxContextTokens = 0 }},
		{"output", func(m *Model) { m.MaxOutputTokens = 2048 }},
		{"training", func(m *Model) { m.TrainsOnCustomerData = true }},
		{"retention", func(m *Model) { m.RetainsPayloads = true }},
		{"no-zdr", func(m *Model) { m.ZeroDataRetentionAvailable = false }},
		{"inactive", func(m *Model) { m.Active = false }},
		{"schema", func(m *Model) { m.StructuredOutput = false }},
		{"stream", func(m *Model) { m.Streaming = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := model("medium")
			tc.change(&m)
			if _, err := planner(t, policy(), m).Plan(principal(), request()); !errors.Is(err, ErrNoRoute) {
				t.Fatal(err)
			}
		})
	}
	r := request()
	r.Target = ptr(contract.ModelTarget("fixture/medium"))
	r.Facts.EstimatedInputTokens = ptr(126976)
	p := policy()
	p.Limits.MaxPricePerRequest = &contract.Money{Amount: "1", Currency: "USD"}
	if _, err := planner(t, p, model("medium")).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("126976+8192 must not fit131072: %v", err)
	}
	r.Facts.EstimatedInputTokens = ptr(122880)
	if _, err := planner(t, p, model("medium")).Plan(principal(), r); err != nil {
		t.Fatal(err)
	}
}
func TestMalformedOrUnsupportedRequestsNeverFallBack(t *testing.T) {
	p := planner(t, policy(), model("medium"))
	for _, change := range []func(*Request){func(r *Request) { r.Operation = contract.GrantAudioSpeech }, func(r *Request) { r.Facts.MaxOutputTokens = ptr(8193) }, func(r *Request) { r.Facts.EstimatedInputTokens = nil }, func(r *Request) { r.Facts.EstimatedInputTokens = ptr(-1) }, func(r *Request) { r.Facts.DeclaredTools = 129 }, func(r *Request) { r.Facts.InputImages = ptr(-1) }, func(r *Request) { r.Facts.AudioInputMilliseconds = ptr(1) }} {
		r := request()
		change(&r)
		if _, err := p.Plan(principal(), r); !errors.Is(err, ErrInvalidRequest) {
			t.Fatal(err)
		}
	}
}
func TestImageBoundsAndDeclaredPrices(t *testing.T) {
	r := request()
	r.Facts.InputImages = ptr(2)
	p := policy()
	p.ImageInput = &ImageInputPolicy{Limits: clone(p.Limits), Routes: clone(p.Routes), MaxImages: 16}
	plan, err := planner(t, p, model("medium")).Plan(principal(), r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.InputTokens != 2500 {
		t.Fatalf("image tokens not bounded: %d", plan.InputTokens)
	}
	m := model("medium")
	m.MaxInputTokensPerImage = 0
	if _, err = planner(t, p, m).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
	r.Facts.InputImages = ptr(int(^uint(0) >> 1))
	if _, err = planner(t, p, model("medium")).Plan(principal(), r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("image multiplication overflow admitted")
	}
}
func TestPolicyMustBeExplicitAndUnambiguous(t *testing.T) {
	p := policy()
	for _, change := range []func(*Policy){func(p *Policy) { p.Routes.Deployments = nil }, func(p *Policy) { p.Limits.MaxFundingTier = nil }, func(p *Policy) { p.Limits.MaxPricePerRequest = nil }, func(p *Policy) { p.MaxOutputTokens = 0 }, func(p *Policy) { p.ExecutionTimeout = 24 * time.Hour }} {
		bad := clone(p)
		change(&bad)
		if _, err := NewPlanner([]Policy{bad}, nil); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	if _, err := NewPlanner([]Policy{p, p}, nil); err == nil {
		t.Fatal("ambiguous enrollment accepted")
	}
}
func TestProviderRegionAndCommercialRestrictionsArePreserved(t *testing.T) {
	p := policy()
	p.Limits.ProviderAllowlist = []contract.ProviderSlug{"fixture"}
	p.Limits.DeniedRegions = []contract.Region{"eu"}
	p.Limits.AllowedLicenseIDs = []string{"reviewed"}
	p.Limits.RequireCommercialUseRights = true
	plan, err := planner(t, p, model("medium")).Plan(principal(), request())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Steps[0].Limits.ProviderAllowlist, p.Limits.ProviderAllowlist) || !slices.Equal(plan.Steps[0].Limits.DeniedRegions, p.Limits.DeniedRegions) {
		t.Fatal("lost deployment restrictions")
	}
	m := model("medium")
	m.CommercialUseAllowed = false
	if _, err = planner(t, p, m).Plan(principal(), request()); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
}

func TestUnknownStructuredCapabilityRequiresCurrentRouteAcceptance(t *testing.T) {
	for _, field := range []string{"structuredOutput", "jsonMode"} {
		t.Run(field, func(t *testing.T) {
			m := model("medium")
			m.StructuredOutput = false
			m.JSONMode = false
			r := request()
			r.StructuredOutput = field == "structuredOutput"
			r.JSONMode = field == "jsonMode"
			accepted := []provider.RequestParameter{provider.ParameterResponseFormat}
			m.AcceptedParameters = accepted
			if _, err := planner(t, policy(), m).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
				t.Fatal("authoritativefalse overridden")
			}
			m.Unreported = []string{field}
			m.AcceptedParameters = nil
			if _, err := planner(t, policy(), m).Plan(principal(), r); !errors.Is(err, ErrNoRoute) {
				t.Fatal("unknown without evidence admitted")
			}
			m.AcceptedParameters = accepted
			plan, err := planner(t, policy(), m).Plan(principal(), r)
			if err != nil {
				t.Fatal(err)
			}
			if plan.AllowsParameters(m.ID, nil) || plan.AllowsParameters(m.ID, &[]provider.RequestParameter{}) {
				t.Fatal("another route inherited line evidence")
			}
			if !plan.AllowsParameters(m.ID, &accepted) {
				t.Fatal("observed acceptance lost")
			}
			if len(plan.RequiredParameters[m.ID]) != 1 {
				t.Fatal("required evidence not persisted")
			}
		})
	}
}
