package nativeadmission

import (
	"github.com/OxyHQ/Kaana/internal/commercial"
	"github.com/OxyHQ/Kaana/internal/contract"
	"math/big"
	"testing"
	"time"
)

func TestReviewedInitialPolicyPreservesVerifiedTerms(t *testing.T) {
	policies, err := LoadPolicyFile("../../configs/native-admission/alia-kaana-v2.json", "eb3385f238ff5639771542b0caeb6cb7474347d59f205400351e6f9c2adcf490")
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

func TestReviewedPriceBookPreservesAllEightExportedTariffs(t *testing.T) {
	policies, err := LoadPolicyFile("../../configs/native-admission/alia-kaana-v2.json", "eb3385f238ff5639771542b0caeb6cb7474347d59f205400351e6f9c2adcf490")
	if err != nil {
		t.Fatal(err)
	}
	p := policies[0]
	expected := map[string][3]string{
		"openai/gpt-oss-120b|cerebras":               {"0.35", "0.35", "0.75"},
		"openai/gpt-oss-120b|groq":                   {"0.15", "0.075", "0.60"},
		"openai/gpt-oss-120b|openrouter":             {"0.03", "0.03", "0.17"},
		"deepseek/deepseek-v4-flash-0731|openrouter": {"0.0434", "0.0434", "0.30"},
		"openai/gpt-5.4-nano|openrouter":             {"0.20", "0.20", "1.25"},
		"anthropic/claude-haiku-4.5|openrouter":      {"1", "1", "5"},
		"anthropic/claude-sonnet-5|openrouter":       {"2", "2", "10"},
	}
	if p.Version != 2 || len(p.PriceBook) != 8 {
		t.Fatal("reviewed schedule count changed")
	}
	for _, entry := range p.PriceBook {
		if entry.SourceArtifactSHA256 != "317618cf04035449b23a43d438e2adbb60b528400cf6237f4fa8e2cde61ba7f3" || entry.SourcePriceVersionID == entry.Price.PriceVersionID {
			t.Fatal("tariff migration provenance missing")
		}
		rates, err := unitRates(entry.Price)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Price.ModelLine == "x-ai/text-to-speech" {
			amount, err := RateUnits(entry.Price, []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: 15000}})
			if err != nil || amount.Amount != "0.225000000000" {
				t.Fatal("speech tariff changed", amount, err)
			}
			continue
		}
		want, ok := expected[string(entry.Price.ModelLine)+"|"+string(entry.Route.Provider)]
		if !ok {
			t.Fatal("unexpected scoped tariff")
		}
		for unit, text := range map[contract.UsageUnit]string{contract.UnitInputTokens: want[0], contract.UnitCachedInputTokens: want[1], contract.UnitOutputTokens: want[2], contract.UnitReasoningTokens: want[2]} {
			expectedRate, _ := new(big.Rat).SetString(text)
			expectedRate.Quo(expectedRate, big.NewRat(1000000, 1))
			if rates[unit] == nil || rates[unit].Cmp(expectedRate) != 0 {
				t.Fatal("verified exported tariff changed", entry.Price.ModelLine, entry.Route.Provider, unit)
			}
		}
		if rates[contract.UnitCacheWriteInputTokens] != nil || rates[contract.UnitRequests] == nil || rates[contract.UnitRequests].Sign() != 0 {
			t.Fatal("undeclared cache-write tariff or request fee invented")
		}
	}
}
