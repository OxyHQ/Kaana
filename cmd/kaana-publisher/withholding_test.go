package main

import (
	"context"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestTheWithholdingPolicyDefaultsAndReadsEveryVariable(t *testing.T) {
	defaults, err := withholdPolicyFromEnv(environment(nil))
	if err != nil || defaults != publisher.DefaultWithholdPolicy() {
		t.Fatalf("defaults = %+v, %v", defaults, err)
	}
	policy, err := withholdPolicyFromEnv(environment(map[string]string{
		"KAANA_PUBLISHER_WITHHOLDING":           "report",
		"KAANA_PUBLISHER_WITHHOLD_FAILURES":     "8",
		"KAANA_PUBLISHER_WITHHOLD_FAILURE_SPAN": "20m",
		"KAANA_PUBLISHER_WITHHOLD_MIN":          "45m",
		"KAANA_PUBLISHER_WITHHOLD_MAX":          "12h",
		"KAANA_PUBLISHER_WITHHOLD_LOOKBACK":     "48h",
	}))
	want := publisher.WithholdPolicy{
		Failures: 8, FailureSpan: 20 * time.Minute, MinQuarantine: 45 * time.Minute,
		MaxQuarantine: 12 * time.Hour, Lookback: 48 * time.Hour, ReportOnly: true,
	}
	if err != nil || policy != want {
		t.Fatalf("policy = %+v, %v; want %+v", policy, err, want)
	}
}

func TestAnUnreadableWithholdingVariableRefusesToStart(t *testing.T) {
	for name, value := range map[string]string{
		"KAANA_PUBLISHER_WITHHOLDING":       "off",
		"KAANA_PUBLISHER_WITHHOLD_FAILURES": "five",
		"KAANA_PUBLISHER_WITHHOLD_MIN":      "half an hour",
		"KAANA_PUBLISHER_WITHHOLD_LOOKBACK": "1h",
	} {
		if _, err := withholdPolicyFromEnv(environment(map[string]string{name: value})); err == nil {
			t.Errorf("%s=%q was accepted", name, value)
		}
	}
}

type fakeEvidenceReader struct {
	read  credentialstore.PublicationEvidence
	asked []contract.ProviderSlug
	since time.Time
}

func (f *fakeEvidenceReader) ReadPublicationEvidence(_ context.Context, providers []contract.ProviderSlug, since time.Time) (credentialstore.PublicationEvidence, error) {
	f.asked, f.since = providers, since
	return f.read, nil
}

func TestStoreEvidenceCarriesEveryFactTheRuleReads(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	reader := &fakeEvidenceReader{read: credentialstore.PublicationEvidence{
		Keys: []credentialstore.PublicationKey{{
			Provider: "cerebras", KeyID: "key-c",
			Runtime: provider.CredentialRuntimeState{Reason: provider.KeyExhausted, RetiredUntil: at.Add(time.Hour)},
			Capacity: []credentialstore.PublicationCapacity{
				{Kind: "balance", ObservedAt: at, FreshUntil: at.Add(time.Hour), BalancePicos: "0"},
				{Kind: "quota", QuotaWindow: "month", QuotaRemaining: "12"},
			},
		}},
		Bindings: []provider.CredentialBinding{{DeploymentID: "dep_a", Provider: "cerebras", KeyID: "key-c"}},
		Failures: []credentialstore.DeploymentFailureStreak{{
			DeploymentID: "dep_a", Provider: "cerebras", KeyID: "key-c", FailureCode: contract.CodeModelNotFound,
			Failures: 4, FirstAt: at.Add(-time.Hour), LastAt: at,
		}},
	}}
	evidence, err := storeEvidence{reader: reader}.PublicationEvidence(context.Background(), []contract.ProviderSlug{"cerebras"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.asked) != 1 || !reader.since.Equal(at) {
		t.Fatalf("asked %v since %s", reader.asked, reader.since)
	}
	key := evidence.Keys["cerebras"][0]
	if key.KeyID != "key-c" || key.Retirement != "exhausted" || !key.RetiredUntil.Equal(at.Add(time.Hour)) ||
		len(key.Capacity) != 2 || !key.Capacity[0].Empty || key.Capacity[1].Empty || key.Capacity[1].Window != "month" {
		t.Fatalf("key = %+v", key)
	}
	if evidence.Bindings["dep_a"] != (publisher.KeyBinding{Provider: "cerebras", KeyID: "key-c"}) {
		t.Fatalf("bindings = %+v", evidence.Bindings)
	}
	streak := evidence.Failures[0]
	if streak.Code != contract.CodeModelNotFound || streak.Count != 4 || !streak.First.Equal(at.Add(-time.Hour)) || !streak.Last.Equal(at) {
		t.Fatalf("streak = %+v", streak)
	}
}

func TestOnlyAZeroAmountIsEmptyCapacity(t *testing.T) {
	for _, check := range []struct {
		observation credentialstore.PublicationCapacity
		empty       bool
	}{
		{credentialstore.PublicationCapacity{Kind: "balance", BalancePicos: "0"}, true},
		{credentialstore.PublicationCapacity{Kind: "balance", BalancePicos: "10"}, false},
		{credentialstore.PublicationCapacity{Kind: "quota", QuotaRemaining: "0"}, true},
		{credentialstore.PublicationCapacity{Kind: "quota", QuotaRemaining: "100"}, false},
		{credentialstore.PublicationCapacity{Kind: "expiry"}, false},
	} {
		if got := check.observation.Empty(); got != check.empty {
			t.Errorf("%+v.Empty() = %v", check.observation, got)
		}
	}
}
