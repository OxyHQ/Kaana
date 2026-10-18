package contract

import (
	"testing"
	"time"
)

// grantFixtures are the grant exchange as Kaana's Go types marshal it: the
// authorization requests Kaana sends, the grants it must read and the
// settlements it delivers. Each is parsed by the contract's own Zod schema
// (tools/contract/validate.mjs), so a value Oxy would refuse is a red test here.
func grantValidFixtures(t *testing.T) []fixture {
	t.Helper()
	attribution := sampleAttribution()
	noFailover := false
	reservation := "rsv_01"
	metered := "mtr_01"
	served := 0
	idempotency := IdempotencyKey("idem-1")
	user := UserID("user_01")
	clientRequestID := "client-1"
	revision := ModelRevisionLabel("2026-05-01")
	line := ModelID("openai/gpt-5")
	level := PowerPro
	xhigh := PowerXHigh
	credit := FundingTierCredit
	estimated, maxOutput, characters := 812, 1024, 120
	kind := RealtimeConversation
	relationship := "alia-kaana"
	restriction := "promotional-only"

	commercial := Grant{
		SchemaVersion: GrantSchemaVersion,
		GrantID:       "grt_01",
		RequestID:     attribution.RequestID,
		Operation:     GrantResponses,
		IssuedAt:      "2026-10-15T10:00:00.000Z",
		ExecuteBy:     "2026-10-15T10:01:00.000Z",
		BodySHA256:    "a" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde",
		Principal: GrantPrincipal{
			BillingAccountID: attribution.Principal.Billing.AccountID,
			ApplicationID:    attribution.Principal.ApplicationID,
			CredentialID:     attribution.Principal.CredentialID,
			Environment:      EnvironmentProduction,
			Scopes:           []Scope{ScopeInvoke},
			Lane:             LaneMachineCredential,
		},
		UserID:      &user,
		Economics:   GrantEconomics{Treatment: TreatmentCommercial, PolicyVersion: "pol-7"},
		Reservation: &GrantReservation{ReservationID: reservation, Ceiling: Money{Amount: "0.25", Currency: "USD"}, FundingRestriction: &restriction},
		Steps: []GrantStep{{
			Target: GrantTarget{Kind: GrantTargetModel, ModelLine: &line, Revision: &revision},
			Limits: GrantStepLimits{
				MaxPricePerRequest:             &Money{Amount: "0.25", Currency: "USD"},
				MaxFundingTier:                 &credit,
				AdmittedModelLines:             []AdmittedModelLine{{ModelLine: line, PriceVersionID: "price-1"}},
				ProviderAllowlist:              []ProviderSlug{"openai"},
				ProviderDenylist:               []ProviderSlug{"groq"},
				AllowedRegions:                 []Region{"us"},
				DeniedRegions:                  []Region{"eu"},
				RequireZeroDataRetention:       true,
				ProhibitTrainingOnCustomerData: true,
				AllowedLicenseIDs:              []string{"proprietary"},
				RequireCommercialUseRights:     true,
				SameModelFailover:              &noFailover,
			},
		}},
		IdempotencyKey: &idempotency,
		Client:         GrantClient{Labels: RequestLabels{"feature": "chat"}},
		RoutingPolicy:  RoutingPolicyReference{RoutingPolicyID: "rp_1", PolicyVersion: 3},
	}
	aliaPro := Grant{
		SchemaVersion: GrantSchemaVersion,
		GrantID:       "grt_02",
		RequestID:     attribution.RequestID,
		Operation:     GrantResponses,
		IssuedAt:      "2026-10-15T10:00:00Z",
		ExecuteBy:     "2026-10-15T10:00:59Z",
		BodySHA256:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Principal: GrantPrincipal{
			BillingAccountID: attribution.Principal.Billing.AccountID,
			ApplicationID:    attribution.Principal.ApplicationID,
			CredentialID:     attribution.Principal.CredentialID,
			Environment:      EnvironmentProduction,
			Scopes:           []Scope{ScopeInvoke},
			Lane:             LaneServiceToken,
		},
		Economics: GrantEconomics{Treatment: TreatmentInternalMetered, PolicyVersion: "pol-7", RelationshipID: &relationship},
		Capacity:  &GrantCapacity{MeteredUsageID: metered},
		Steps: []GrantStep{
			{Target: GrantTarget{Kind: GrantTargetLevel, Level: &level}, Limits: GrantStepLimits{
				MaxPricePerRequest: &Money{Amount: "1.00", Currency: "USD"}, MaxFundingTier: &credit,
				AdmittedModelLines: []AdmittedModelLine{{ModelLine: "anthropic/claude-opus-4", PriceVersionID: "price-2"}},
			}},
			{Target: GrantTarget{Kind: GrantTargetLevel, Level: &xhigh}, Limits: GrantStepLimits{
				MaxPricePerRequest: &Money{Amount: "0.05", Currency: "USD"},
				AdmittedModelLines: []AdmittedModelLine{{ModelLine: "openai/gpt-oss-120b", PriceVersionID: "price-3"}},
			}},
		},
		Client:        GrantClient{},
		RoutingPolicy: RoutingPolicyReference{RoutingPolicyID: "rp_1", PolicyVersion: 3},
	}
	for _, grant := range []Grant{commercial, aliaPro} {
		if err := grant.Validate(); err != nil {
			t.Fatalf("a valid grant fixture fails Go's own validation: %v", err)
		}
	}

	inference := InferenceAuthorizationRequest{
		SchemaVersion: AuthorizationSchemaVersion,
		Kind:          AuthorizationKindInference,
		Credential:    "oxy_sk_test_credential",
		RequestID:     attribution.RequestID,
		Operation:     GrantRealtimeSession,
		Target:        &GrantTarget{Kind: GrantTargetModel, ModelLine: &line, Revision: &revision},
		Facts: GrantRequestFacts{
			EstimatedInputTokens: &estimated, MaxOutputTokens: &maxOutput, InputCharacters: &characters,
			AudioInputMilliseconds: pointerTo(1500), EmbeddingInputs: pointerTo(2), Images: pointerTo(1),
			DeclaredTools: 2, Stream: true, RealtimeSessionKind: &kind,
		},
		IdempotencyKey:   &idempotency,
		OnBehalfOfUserID: &user,
		BodySHA256:       commercial.BodySHA256,
		Client: AuthorizationClient{
			Endpoint: "/v1/realtime", ClientRequestID: &clientRequestID,
			ReceivedAt: "2026-10-15T10:00:00.000Z", Labels: RequestLabels{"feature": "voice"},
		},
	}
	generation := GenerationID("gen_01")
	report := UsageReport{
		SchemaVersion: UsageReportSchemaVersion, RequestID: attribution.RequestID, GenerationID: &generation,
		Attribution: commercial.Attribution(), Outcome: OutcomeCompleted,
		Units:       []UsageQuantity{{Unit: UnitInputTokens, Quantity: 812}, {Unit: UnitOutputTokens, Quantity: 64}},
		UsageSource: UsageProviderReported, ResolvedModelReference: "openai/gpt-5@2026-05-01",
		ServingProvider: "openai", DeploymentID: "dep_openai_gpt5", RouteSwitches: 0, FundingTier: &credit,
		StartedAt: "2026-10-15T10:00:00.100Z", CompletedAt: "2026-10-15T10:00:02.000Z", TimeToFirstTokenMs: pointerTo(240),
	}
	failed := report
	failed.Outcome, failed.Units, failed.FundingTier, failed.TimeToFirstTokenMs = OutcomeFailed, []UsageQuantity{}, nil, nil
	settled := Settlement{
		SchemaVersion: SettlementSchemaVersion, SettlementID: "stl_01", GrantID: commercial.GrantID,
		RequestID: attribution.RequestID, ReservationID: &reservation, MeteredUsageID: &metered,
		ServedStep: &served, Report: report, CreatedAt: "2026-10-15T10:00:02.001Z",
	}
	unserved := Settlement{
		SchemaVersion: SettlementSchemaVersion, SettlementID: "stl_02", GrantID: aliaPro.GrantID,
		RequestID: attribution.RequestID, MeteredUsageID: &metered, Report: failed,
		CreatedAt: Timestamp(time.Date(2026, 10, 15, 10, 0, 3, 0, time.UTC).Format(time.RFC3339Nano)),
	}
	for _, settlement := range []Settlement{settled, unserved} {
		if err := settlement.Validate(); err != nil {
			t.Fatalf("a valid settlement fixture fails Go's own validation: %v", err)
		}
	}
	next := "c_42"
	return []fixture{
		{Schema: "grantSchema", Case: "commercial-every-field", Value: commercial},
		{Schema: "grantSchema", Case: "alia-pro-two-steps", Value: aliaPro},
		{Schema: "authorizationRequestSchema", Case: "inference-every-field", Value: inference},
		{Schema: "authorizationRequestSchema", Case: "catalogue-read", Value: CatalogueAuthorizationRequest{
			SchemaVersion: AuthorizationSchemaVersion, Kind: AuthorizationKindCatalogue, Credential: "oxy_sk_test_credential", RequestID: attribution.RequestID,
		}},
		{Schema: "authorizationRequestSchema", Case: "receipt-read-by-key", Value: ReceiptAuthorizationRequest{
			SchemaVersion: AuthorizationSchemaVersion, Kind: AuthorizationKindReceipt, Credential: "oxy_sk_test_credential",
			RequestID: attribution.RequestID, IdempotencyKey: &idempotency, OnBehalfOfUserID: &user,
		}},
		{Schema: "catalogueAccessSchema", Case: "levels", Value: CatalogueAccess{
			SchemaVersion: AuthorizationSchemaVersion, Kind: AuthorizationKindCatalogue, Audience: AudiencePublic,
			AllowedLevels: []PowerLevel{PowerAuto, PowerHigh}, ExpiresAt: "2026-10-15T10:01:00Z",
		}},
		{Schema: "settlementSchema", Case: "completed", Value: settled},
		{Schema: "settlementSchema", Case: "failed-before-output", Value: unserved},
		{Schema: "settlementPageSchema", Case: "page", Value: SettlementPage{Data: []Settlement{settled, unserved}, NextCursor: &next}},
		{Schema: "settlementAcknowledgementSchema", Case: "duplicate", Value: SettlementAcknowledgement{
			SchemaVersion: SettlementSchemaVersion, SettlementID: "stl_01", Status: SettlementDuplicate,
		}},
	}
}

// grantInvalidFixtures are values the contract must REFUSE, each also refused
// by the Go validation that guards the same rule.
// commercialFixtures are the deployment descriptor's `commercial` member
// (contract set 4.4.0): two that must parse and two that break its rules.
func commercialFixtures(t *testing.T) (valid, invalid []fixture) {
	t.Helper()
	dedicated := true
	plain := DeploymentCommercial{AvailabilityScope: ScopePlatformInternal, CommercialPermission: PermissionStandardApplicationUse}
	reserved := DeploymentCommercial{AvailabilityScope: ScopeEnterprise, CommercialPermission: PermissionWholesaleContract, DedicatedCapacity: &dedicated}
	byokWithoutByok := DeploymentCommercial{AvailabilityScope: ScopeBYOKOnly, CommercialPermission: PermissionStandardApplicationUse}
	paygWithoutResale := DeploymentCommercial{AvailabilityScope: ScopePublicPAYG, CommercialPermission: PermissionStandardApplicationUse}
	for _, ok := range []DeploymentCommercial{plain, reserved} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("a valid commercial fixture fails Go's own validation: %v", err)
		}
	}
	for _, bad := range []DeploymentCommercial{byokWithoutByok, paygWithoutResale} {
		if bad.Validate() == nil {
			t.Fatalf("Go accepts %+v", bad)
		}
	}
	accepted := []fixture{
		{Schema: "deploymentCommercialSchema", Case: "platform-internal", Value: plain},
		{Schema: "deploymentCommercialSchema", Case: "enterprise-dedicated", Value: reserved},
	}
	refused := []fixture{
		{Schema: "deploymentCommercialSchema", Case: "byok-scope-without-byok-permission", Value: byokWithoutByok},
		{Schema: "deploymentCommercialSchema", Case: "public-payg-without-resale", Value: paygWithoutResale},
	}
	return accepted, refused
}

func grantInvalidFixtures(t *testing.T) []fixture {
	t.Helper()
	valid := grantValidFixtures(t)
	grant := valid[0].Value.(Grant)
	late := grant
	late.ExecuteBy = "2026-10-15T10:01:00.001Z"
	if late.Validate() == nil {
		t.Fatal("Go accepts a grant executable for more than 60 seconds")
	}
	unheld := grant
	unheld.Reservation = nil
	if unheld.Validate() == nil {
		t.Fatal("Go accepts a charged grant without its hold")
	}
	settlement := valid[6].Value.(Settlement)
	unserved := settlement
	unserved.ServedStep = nil
	if unserved.Validate() == nil {
		t.Fatal("Go accepts a completed settlement no step served")
	}
	return []fixture{
		{Schema: "grantSchema", Case: "execution-window-over-60s", Value: late},
		{Schema: "grantSchema", Case: "charged-without-hold", Value: unheld},
		{Schema: "settlementSchema", Case: "completed-without-served-step", Value: unserved},
	}
}
