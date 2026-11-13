package nativeadmission

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestPromotionalAdmissionEvidence(t *testing.T) {
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	slug := contract.ProviderSlug("example")
	key := CredentialFundingEvidence{Provider: slug, KeyID: "promotion", CapacityCategory: "promotional"}
	check := func(t *testing.T, evidence []CredentialFundingEvidence, ids []string, readAt, now time.Time, want bool) {
		t.Helper()
		if got := PromotionalCreditAvailable(evidence, slug, ids, readAt, now); got != want {
			t.Fatalf("eligible = %v, want %v", got, want)
		}
	}
	check(t, []CredentialFundingEvidence{key}, []string{key.KeyID}, at, at, true)
	check(t, []CredentialFundingEvidence{key}, nil, at, at, false)
	check(t, []CredentialFundingEvidence{key}, []string{"other"}, at, at, false)
	check(t, []CredentialFundingEvidence{key}, []string{key.KeyID}, time.Time{}, at, false)
	check(t, []CredentialFundingEvidence{key}, []string{key.KeyID}, at.Add(time.Second), at, false)
	check(t, []CredentialFundingEvidence{key}, []string{key.KeyID}, at, at.Add(6*time.Hour), false)
	check(t, []CredentialFundingEvidence{key}, []string{key.KeyID}, at, at.Add(6*time.Hour-time.Millisecond), true)
	for _, category := range []string{"", "trial", "paid", "prepaid"} {
		t.Run(category, func(t *testing.T) {
			other := key
			other.CapacityCategory = category
			check(t, []CredentialFundingEvidence{other}, []string{key.KeyID}, at, at, false)
		})
	}
	other := key
	other.Provider = "another"
	check(t, []CredentialFundingEvidence{other}, []string{key.KeyID}, at, at, false)
}

func TestPromotionalCapacityAndExpiry(t *testing.T) {
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		capacity FundingCapacity
		want     bool
	}{
		{"fresh zero balance", FundingCapacity{Kind: "balance", BalancePicos: "000", FreshUntil: at.Add(time.Hour)}, false},
		{"fresh zero quota", FundingCapacity{Kind: "quota", QuotaRemaining: "0", QuotaWindow: "minute", FreshUntil: at.Add(time.Hour)}, false},
		{"monthly zero", FundingCapacity{Kind: "quota", QuotaRemaining: "0.0", QuotaWindow: "month"}, false},
		{"stale zero balance", FundingCapacity{Kind: "balance", BalancePicos: "0", FreshUntil: at}, true},
		{"stale zero quota", FundingCapacity{Kind: "quota", QuotaRemaining: "0", FreshUntil: at}, true},
		{"positive balance", FundingCapacity{Kind: "balance", BalancePicos: "100", FreshUntil: at.Add(time.Hour)}, true},
		{"positive quota", FundingCapacity{Kind: "quota", QuotaRemaining: "1"}, true},
		{"expired", FundingCapacity{Kind: "expiry", ExpiresAt: at.Add(-time.Second)}, false},
		{"future expiry", FundingCapacity{Kind: "expiry", ExpiresAt: at.Add(time.Hour)}, true},
		{"unknown capacity", FundingCapacity{Kind: "unknown"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := CredentialFundingEvidence{Provider: "example", KeyID: "promotion", CapacityCategory: "promotional", Capacity: []FundingCapacity{test.capacity}}
			if got := PromotionalCreditAvailable([]CredentialFundingEvidence{key}, "example", []string{"promotion"}, at, at); got != test.want {
				t.Fatalf("eligible = %v, want %v", got, test.want)
			}
		})
	}
	// Cached economics cannot renew a previously known expiry or the read TTL.
	key := CredentialFundingEvidence{Provider: "example", KeyID: "promotion", CapacityCategory: "promotional", Capacity: []FundingCapacity{{Kind: "expiry", ExpiresAt: at.Add(time.Minute), FreshUntil: at.Add(time.Second)}}}
	if PromotionalCreditAvailable([]CredentialFundingEvidence{key}, "example", []string{key.KeyID}, at, at.Add(time.Minute)) {
		t.Fatal("cached observation extended an earlier grant expiry")
	}
	// One exhausted key does not hide another available grant in the same pool.
	available := CredentialFundingEvidence{Provider: "example", KeyID: "second", CapacityCategory: "promotional"}
	if !PromotionalCreditAvailable([]CredentialFundingEvidence{key, available}, "example", []string{key.KeyID, available.KeyID}, at, at.Add(time.Minute)) {
		t.Fatal("available second grant was ignored")
	}
	// The admission gate must not silently narrow the executor's existing tier.
	if !provider.FundingTierCredit.Admits(provider.FundingFree) || !provider.FundingTierCredit.Admits(provider.FundingPrepaid) {
		t.Fatal("existing credit execution ceiling changed")
	}
}
