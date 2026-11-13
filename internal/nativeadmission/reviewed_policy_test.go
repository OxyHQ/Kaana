package nativeadmission

import (
	"github.com/OxyHQ/Kaana/internal/commercial"
	"github.com/OxyHQ/Kaana/internal/contract"
	"testing"
	"time"
)

func TestReviewedInitialPolicyPreservesVerifiedTerms(t *testing.T) {
	policies, err := LoadPolicyFile("../../configs/native-admission/alia-kaana-v1.json", "682ee01191981e4ceaff0ec163061ef99ac02b8598c34977da18ce293b56bfa2")
	if err != nil || len(policies) != 1 {
		t.Fatal("reviewed policy invalid")
	}
	p := policies[0]
	if p.EconomicTreatment != InternalMetered || len(p.CredentialIDs) != 1 || p.MaxConcurrent != 32 || p.MaxPerUTCDay != 5000 || p.MaxInputTokens != 126976 || p.MaxOutputTokens != 8192 || p.DefaultOutputTokens != 8192 || p.ExecutionTimeout != 15*time.Minute {
		t.Fatal("reviewed population limits changed")
	}
	for _, limits := range []contract.GrantStepLimits{p.Limits, p.ImageInput.Limits, p.Speech.Limits, p.Transcription.Limits, p.Realtime.Limits} {
		if limits.MaxFundingTier != nil || limits.RequireZeroDataRetention || limits.ProhibitTrainingOnCustomerData || limits.RequireCommercialUseRights || len(limits.ProviderAllowlist)+len(limits.ProviderDenylist)+len(limits.AllowedRegions)+len(limits.DeniedRegions)+len(limits.AllowedLicenseIDs) != 0 || !limits.SameModelFailoverAllowed() {
			t.Fatal("verified unconstrained policy was narrowed or widened")
		}
	}
	if p.Limits.MaxPricePerRequest.Amount != "0.05" || p.ImageInput.Limits.MaxPricePerRequest.Amount != "0.15" || p.ImageInput.MaxImages != 16 || p.Promotional.Limits.MaxPricePerRequest.Amount != "1.00" || p.Promotional.Limits.MaxFundingTier == nil || *p.Promotional.Limits.MaxFundingTier != contract.FundingTierCredit || !p.Promotional.Routes.RequirePromotionalCredit || p.Promotional.FallbackLevel != contract.PowerXHigh {
		t.Fatal("reviewed text or promotional terms changed")
	}
	if p.Speech.MaxInput != 15000 || p.Speech.Limits.MaxPricePerRequest.Amount != "0.25" || p.Transcription.MaxInput != 300000 || p.Transcription.Limits.MaxPricePerRequest.Amount != "0.05" || p.Realtime.Limits.MaxPricePerRequest.Amount != "0.25" || p.Realtime.MaxSessionDuration != 30*time.Minute || p.Realtime.MaxSessionResponses != 500 {
		t.Fatal("reviewed voice limits changed")
	}
	declarations, err := commercial.Load("../../configs/deployment-commercial.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []RouteScope{p.Routes, p.ImageInput.Routes, p.Speech.Routes} {
		for _, route := range scope.Deployments {
			facts := declarations.For(route.DeploymentID)
			if facts == nil || facts.Validate() != nil || facts.AvailabilityScope != contract.ScopePlatformInternal {
				t.Fatal("reviewed exact route missing commercial authority")
			}
		}
	}
	digest, err := PolicyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reviewed policy digest: %s", digest)
}
