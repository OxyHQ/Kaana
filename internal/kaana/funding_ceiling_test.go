package kaana_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// poolAdapter serves its platform pool by a real walk: keys named in spent
// answer Anthropic's billing refusal, every other key serves.
func poolAdapter(t *testing.T, spent map[string]bool, tried *[]string, declarations ...provider.KeyDeclaration) (*scriptedAdapter, *provider.KeyPool) {
	t.Helper()
	pool, err := provider.NewKeyPool("stub", declarations, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &scriptedAdapter{slug: "stub", platformPool: pool, streamWithCredentials: func(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
		_, key, err := provider.WalkAttempts(ctx, credentials, call, func(_ context.Context, key provider.Key) (struct{}, provider.CredentialedAttempt) {
			*tried = append(*tried, key.ID)
			if spent[key.ID] {
				return struct{}{}, provider.CredentialedAttempt{Failure: credentialBillingRefused("stub")}
			}
			return struct{}{}, provider.CredentialedAttempt{Accepted: true}
		})
		if err != nil {
			return provider.Outcome{KeyID: key.ID}, err
		}
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "served by "+key.ID); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
		return provider.Outcome{KeyID: key.ID, Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}, pool
}

func ceilinged(tier contract.FundingTier) *contract.Request {
	request := authorizedRequest()
	request.AuthorizedRoutes[0].MaxFundingTier = &tier
	return request
}

var grantAndCard = []provider.KeyDeclaration{
	{KeyID: "grant", Secret: "kaana-test-platform-secret-grant", Funding: provider.FundingPromotional},
	{KeyID: "card", Secret: "kaana-test-platform-secret-card", Funding: provider.FundingPaid},
}

// Oxy ADR 0038: a route authorized on credit only is served on the grant, and
// the usage report says credit served — never which key.
func TestACreditOnlyRouteIsServedOnCreditAndSaysSo(t *testing.T) {
	var tried []string
	adapter, _ := poolAdapter(t, nil, &tried, grantAndCard...)
	_, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{adapter, succeedingAdapter("backup", 7)},
		unbound:     map[contract.DeploymentID]bool{"dep_a": true},
	}.run(t, ceilinged(contract.FundingTierCredit))
	if result.Failure != nil || result.Report == nil {
		t.Fatalf("failure=%v", result.Failure)
	}
	if strings.Join(tried, ",") != "grant" || result.Report.DeploymentID != "dep_a" {
		t.Fatalf("tried %v on %s", tried, result.Report.DeploymentID)
	}
	if result.Report.FundingTier == nil || *result.Report.FundingTier != contract.FundingTierCredit {
		t.Fatalf("served tier = %v", result.Report.FundingTier)
	}
}

// The grant is spent: the request never reaches the paid key of the same
// provider. It fails over to the next signed route instead.
func TestASpentGrantNeverFallsThroughToAPaidKeyUnderACeiling(t *testing.T) {
	var tried []string
	adapter, _ := poolAdapter(t, map[string]bool{"grant": true}, &tried, grantAndCard...)
	backup := succeedingAdapter("backup", 7)
	_, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{adapter, backup},
		unbound:     map[contract.DeploymentID]bool{"dep_a": true},
	}.run(t, ceilinged(contract.FundingTierCredit))
	if result.Failure != nil || result.Report == nil {
		t.Fatalf("failure=%v", result.Failure)
	}
	if strings.Join(tried, ",") != "grant" {
		t.Fatalf("keys tried %v: the paid key was walked under a credit ceiling", tried)
	}
	if result.Report.DeploymentID != "dep_b" || backup.attempts() != 1 {
		t.Fatalf("served on %s; the signed failover should serve", result.Report.DeploymentID)
	}

	// The control: without the ceiling the same spent grant moves to the card.
	var unbounded []string
	control, _ := poolAdapter(t, map[string]bool{"grant": true}, &unbounded, grantAndCard...)
	_, result = harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{control, succeedingAdapter("backup", 7)},
		unbound:     map[contract.DeploymentID]bool{"dep_a": true},
	}.run(t, authorizedRequest())
	if result.Failure != nil || strings.Join(unbounded, ",") != "grant,card" || result.Report.DeploymentID != "dep_a" {
		t.Fatalf("without a ceiling: tried %v, %v", unbounded, result.Failure)
	}
	if result.Report.FundingTier == nil || *result.Report.FundingTier != contract.FundingTierPaid {
		t.Fatalf("served tier = %v, want paid", result.Report.FundingTier)
	}
}

// A pool with no key inside the ceiling is skipped before anything is sent.
func TestARouteWithNoKeyInsideItsCeilingIsSkipped(t *testing.T) {
	var tried []string
	adapter, _ := poolAdapter(t, nil, &tried, provider.KeyDeclaration{KeyID: "card", Secret: "kaana-test-platform-secret-card", Funding: provider.FundingPaid})
	_, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{adapter, succeedingAdapter("backup", 7)},
		unbound:     map[contract.DeploymentID]bool{"dep_a": true},
	}.run(t, ceilinged(contract.FundingTierCredit))
	if result.Failure != nil || len(tried) != 0 || result.Report.DeploymentID != "dep_b" {
		t.Fatalf("tried %v, served %v, %v", tried, result.Report, result.Failure)
	}
}

// A key nobody described has no tier: the report says nothing about it.
func TestAnUnstatedKeyReportsNoTier(t *testing.T) {
	var tried []string
	adapter, _ := poolAdapter(t, nil, &tried, provider.KeyDeclaration{KeyID: "unknown", Secret: "kaana-test-platform-secret-unknown"})
	_, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{adapter, succeedingAdapter("backup", 7)},
		unbound:     map[contract.DeploymentID]bool{"dep_a": true},
	}.run(t, authorizedRequest())
	if result.Failure != nil || result.Report.FundingTier != nil {
		t.Fatalf("tier = %v, %v", result.Report.FundingTier, result.Failure)
	}
}
