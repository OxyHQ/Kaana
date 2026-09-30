package publisher

import (
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

var decideNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

const (
	decideDeployment contract.DeploymentID = "dep_cerebras_gpt_oss_120b_observed_2026_08_19"
	decideProvider   contract.ProviderSlug = "cerebras"
)

func oneKey(key KeyEvidence) Evidence {
	return Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{decideProvider: {key}}}
}

func streak(code contract.ErrorCode, count int, first, last time.Duration) FailureStreak {
	return FailureStreak{
		DeploymentID: decideDeployment, Provider: decideProvider, KeyID: "key-a", Code: code,
		Count: count, First: decideNow.Add(-first), Last: decideNow.Add(-last),
	}
}

func decide(t *testing.T, evidence Evidence) (Withholding, bool) {
	t.Helper()
	return NewDecider(evidence, DefaultWithholdPolicy(), decideNow).Decide(decideDeployment, decideProvider)
}

// The positive control every other case is read against: a usable key and no
// streak publishes. A rule that withheld this would withhold the catalogue.
func TestAUsableKeyWithNoStreakIsPublished(t *testing.T) {
	if decision, withheld := decide(t, oneKey(KeyEvidence{KeyID: "key-a"})); withheld {
		t.Fatalf("a healthy deployment was withheld: %+v", decision)
	}
}

func TestARetiredKeyWithholdsItsDeploymentUntilItReturns(t *testing.T) {
	returns := decideNow.Add(10 * time.Minute)
	decision, withheld := decide(t, oneKey(KeyEvidence{KeyID: "key-a", Retirement: "exhausted", RetiredUntil: returns}))
	if !withheld || decision.Reason != WithheldKeyRetired || !decision.Until.Equal(returns) || decision.KeyID != "key-a" {
		t.Fatalf("decision = %+v, %v", decision, withheld)
	}

	// Its retirement over, the key is a recovery candidate: the state alone no
	// longer withholds it, or a withheld key could never prove it recovered.
	expired := oneKey(KeyEvidence{KeyID: "key-a", Retirement: "exhausted", RetiredUntil: decideNow.Add(-time.Second)})
	if decision, withheld := decide(t, expired); withheld {
		t.Fatalf("an expired retirement still withholds: %+v", decision)
	}
}

func TestADeploymentIsJudgedByTheExactKeyItExecutesOn(t *testing.T) {
	keys := []KeyEvidence{
		{KeyID: "key-a"},
		{KeyID: "key-b", Retirement: "rejected", RetiredUntil: decideNow.Add(time.Hour)},
	}
	evidence := Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{decideProvider: keys}}

	evidence.Bindings = map[contract.DeploymentID]KeyBinding{decideDeployment: {Provider: decideProvider, KeyID: "key-a"}}
	if decision, withheld := decide(t, evidence); withheld {
		t.Fatalf("a deployment bound to a usable key was withheld over its sibling: %+v", decision)
	}
	evidence.Bindings = map[contract.DeploymentID]KeyBinding{decideDeployment: {Provider: decideProvider, KeyID: "key-b"}}
	if decision, withheld := decide(t, evidence); !withheld || decision.KeyID != "key-b" {
		t.Fatalf("a deployment bound to a retired key was published: %+v", decision)
	}
}

// Unbound on a several-key provider, serving refuses the deployment whatever
// is published, and withholding it would hide the id an operator needs to bind.
func TestAnUnboundDeploymentIsWithheldOnlyWhenItsProviderHasNoUsableKey(t *testing.T) {
	early, late := decideNow.Add(5*time.Minute), decideNow.Add(time.Hour)
	partly := Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{decideProvider: {
		{KeyID: "key-a", Retirement: "exhausted", RetiredUntil: late},
		{KeyID: "key-b"},
	}}}
	if decision, withheld := decide(t, partly); withheld {
		t.Fatalf("an unbound deployment was withheld while its provider held a usable key: %+v", decision)
	}
	none := Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{decideProvider: {
		{KeyID: "key-a", Retirement: "exhausted", RetiredUntil: late},
		{KeyID: "key-b", Retirement: "rejected", RetiredUntil: early},
	}}}
	decision, withheld := decide(t, none)
	if !withheld || decision.Reason != WithheldNoUsableKey || !decision.Until.Equal(early) {
		t.Fatalf("decision = %+v, %v; want no_usable_key until the earliest return", decision, withheld)
	}
}

func TestABindingToAnotherProviderResolvesNothing(t *testing.T) {
	evidence := oneKey(KeyEvidence{KeyID: "key-a", Retirement: "exhausted", RetiredUntil: decideNow.Add(time.Hour)})
	evidence.Keys["groq"] = []KeyEvidence{{KeyID: "key-g"}}
	evidence.Bindings = map[contract.DeploymentID]KeyBinding{decideDeployment: {Provider: "groq", KeyID: "key-g"}}
	// Unresolved, so the provider rule applies: cerebras's only key is retired.
	if decision, withheld := decide(t, evidence); !withheld || decision.Reason != WithheldNoUsableKey {
		t.Fatalf("decision = %+v, %v", decision, withheld)
	}
}

func TestFreshEmptyCapacityWithholdsAndStaleOrThrottleCapacityDoesNot(t *testing.T) {
	fresh := decideNow.Add(2 * time.Hour)
	for _, check := range []struct {
		name     string
		evidence CapacityEvidence
		withheld bool
	}{
		{"fresh zero balance", CapacityEvidence{Kind: "balance", Empty: true, FreshUntil: fresh}, true},
		{"fresh positive balance", CapacityEvidence{Kind: "balance", FreshUntil: fresh}, false},
		{"stale zero balance", CapacityEvidence{Kind: "balance", Empty: true, FreshUntil: decideNow.Add(-time.Minute)}, false},
		{"zero balance nobody said is current", CapacityEvidence{Kind: "balance", Empty: true}, false},
		{"monthly quota at zero", CapacityEvidence{Kind: "quota", Window: "month", Empty: true, FreshUntil: fresh}, true},
		{"daily quota at zero", CapacityEvidence{Kind: "quota", Window: "day", Empty: true, FreshUntil: fresh}, true},
		{"per-minute quota at zero is a throttle", CapacityEvidence{Kind: "quota", Window: "minute", Empty: true, FreshUntil: fresh}, false},
		{"hourly quota at zero is a throttle", CapacityEvidence{Kind: "quota", Window: "hour", Empty: true, FreshUntil: fresh}, false},
		{"passed expiry", CapacityEvidence{Kind: "expiry", ExpiresAt: decideNow.Add(-time.Hour), FreshUntil: fresh}, true},
		{"future expiry", CapacityEvidence{Kind: "expiry", ExpiresAt: decideNow.Add(time.Hour), FreshUntil: fresh}, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			decision, withheld := decide(t, oneKey(KeyEvidence{KeyID: "key-a", Capacity: []CapacityEvidence{check.evidence}}))
			if withheld != check.withheld {
				t.Fatalf("withheld = %v (%+v), want %v", withheld, decision, check.withheld)
			}
			if withheld && (decision.Reason != WithheldCapacityExhausted || !decision.Until.Equal(fresh)) {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

// One refusal is the provider's own report about the key, so it needs no
// threshold; the quarantine is the streak's own length, clamped.
func TestACredentialRefusalQuarantinesForTheStreaksOwnLengthClamped(t *testing.T) {
	policy := DefaultWithholdPolicy()
	for _, check := range []struct {
		name        string
		first, last time.Duration
		quarantine  time.Duration
	}{
		{"one refusal takes the minimum", 5 * time.Minute, 5 * time.Minute, policy.MinQuarantine},
		{"a two-hour streak waits two hours", 2*time.Hour + 5*time.Minute, 5 * time.Minute, 2 * time.Hour},
		{"a day-long streak is capped", 23 * time.Hour, 5 * time.Minute, policy.MaxQuarantine},
	} {
		t.Run(check.name, func(t *testing.T) {
			evidence := oneKey(KeyEvidence{KeyID: "key-a"})
			evidence.Failures = []FailureStreak{streak(contract.CodeProviderBillingRefused, 1, check.first, check.last)}
			decision, withheld := decide(t, evidence)
			want := decideNow.Add(-check.last).Add(check.quarantine)
			if !withheld || decision.Reason != WithheldCredentialRefused || !decision.Until.Equal(want) || decision.Failures != 1 {
				t.Fatalf("decision = %+v, %v; want credential_refused until %s", decision, withheld, want)
			}
		})
	}

	// Past the quarantine it is published for a trial: nothing else can ever
	// prove the account was funded again.
	evidence := oneKey(KeyEvidence{KeyID: "key-a"})
	evidence.Failures = []FailureStreak{streak(contract.CodeProviderCredentialInvalid, 1, 31*time.Minute, 31*time.Minute)}
	if decision, withheld := decide(t, evidence); withheld {
		t.Fatalf("a refusal past its quarantine still withholds: %+v", decision)
	}
}

func TestCapacityRecordedAfterTheRefusalLiftsTheQuarantine(t *testing.T) {
	refused := decideNow.Add(-5 * time.Minute)
	evidence := oneKey(KeyEvidence{KeyID: "key-a", Capacity: []CapacityEvidence{{
		Kind: "balance", ObservedAt: refused.Add(time.Minute), FreshUntil: decideNow.Add(time.Hour),
	}}})
	evidence.Failures = []FailureStreak{streak(contract.CodeProviderBillingRefused, 1, 5*time.Minute, 5*time.Minute)}
	if decision, withheld := decide(t, evidence); withheld {
		t.Fatalf("a top-up recorded after the refusal did not lift it: %+v", decision)
	}

	// The same balance observed BEFORE the refusal says nothing about it.
	evidence.Keys[decideProvider][0].Capacity[0].ObservedAt = refused.Add(-time.Minute)
	if _, withheld := decide(t, evidence); !withheld {
		t.Fatal("a balance older than the refusal lifted it")
	}
}

func TestSustainedProviderSideFailuresWithholdAndTransientOnesNever(t *testing.T) {
	policy := DefaultWithholdPolicy()
	for _, check := range []struct {
		name     string
		streaks  []FailureStreak
		withheld bool
	}{
		{"five server errors over ten minutes", []FailureStreak{streak(contract.CodeProviderError, 5, 12*time.Minute, 2*time.Minute)}, true},
		{"mixed provider-side codes add up", []FailureStreak{
			streak(contract.CodeProviderError, 3, 12*time.Minute, 4*time.Minute),
			streak(contract.CodeModelNotFound, 2, 8*time.Minute, 2*time.Minute),
		}, true},
		{"account-level permission refusals", []FailureStreak{streak(contract.CodePermissionDenied, 6, 30*time.Minute, time.Minute)}, true},
		{"one short of the threshold", []FailureStreak{streak(contract.CodeProviderError, 4, 30*time.Minute, time.Minute)}, false},
		{"a burst inside one bad minute", []FailureStreak{streak(contract.CodeProviderError, 50, 3*time.Minute, 2*time.Minute)}, false},
		{"a sustained throttle", []FailureStreak{streak(contract.CodeRateLimited, 500, 3*time.Hour, time.Minute)}, false},
		{"a sustained overload", []FailureStreak{streak(contract.CodeProviderOverloaded, 500, 3*time.Hour, time.Minute)}, false},
		{"sustained timeouts", []FailureStreak{streak(contract.CodeProviderTimeout, 500, 3*time.Hour, time.Minute)}, false},
		{"sustained request faults", []FailureStreak{streak(contract.CodeInvalidRequest, 500, 3*time.Hour, time.Minute)}, false},
		{"a code this build never named", []FailureStreak{streak("provider_on_fire", 500, 3*time.Hour, time.Minute)}, false},
		{"an old streak past its quarantine", []FailureStreak{streak(contract.CodeProviderError, 5, 50*time.Minute, 40*time.Minute)}, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			evidence := oneKey(KeyEvidence{KeyID: "key-a"})
			evidence.Failures = check.streaks
			decision, withheld := NewDecider(evidence, policy, decideNow).Decide(decideDeployment, decideProvider)
			if withheld != check.withheld {
				t.Fatalf("withheld = %v (%+v), want %v", withheld, decision, check.withheld)
			}
			if withheld && decision.Reason != WithheldDeploymentFailing {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

// A streak recorded on the key a deployment USED to be bound to is evidence
// about that key, not the one it executes on now.
func TestAStreakOnAnotherKeyIsNotThisDeploymentsEvidence(t *testing.T) {
	evidence := oneKey(KeyEvidence{KeyID: "key-a"})
	old := streak(contract.CodeProviderBillingRefused, 1, time.Minute, time.Minute)
	old.KeyID = "key-previous"
	evidence.Failures = []FailureStreak{old}
	if decision, withheld := decide(t, evidence); withheld {
		t.Fatalf("another key's refusal withheld this deployment: %+v", decision)
	}
}

// classifyFailure is closed; this pins exactly which contract codes count, so
// widening it is a visible decision rather than a drive-by.
func TestExactlyTheseFailureCodesCount(t *testing.T) {
	counted := map[contract.ErrorCode]failureClass{
		contract.CodeProviderBillingRefused:    failureCredential,
		contract.CodeProviderCredentialInvalid: failureCredential,
		contract.CodeModelNotFound:             failureDeployment,
		contract.CodePermissionDenied:          failureDeployment,
		contract.CodeProviderError:             failureDeployment,
	}
	for _, code := range []contract.ErrorCode{
		contract.CodeInvalidRequest, contract.CodeAuthenticationFailed, contract.CodePermissionDenied,
		contract.CodeInsufficientScope, contract.CodeModelNotFound, contract.CodeUnsupportedModality,
		contract.CodeContextLengthExceeded, contract.CodeRequestTooLarge, contract.CodeOutputLimitExceeded,
		contract.CodeIdempotencyConflict, contract.CodeInsufficientBalance, contract.CodeSpendingLimitExceeded,
		contract.CodeQuotaExceeded, contract.CodeBYOKCredentialInvalid, contract.CodePolicyViolation,
		contract.CodeCommercialPermissionDenied, contract.CodeNoRouteAvailable, contract.CodeUpstreamContentFiltered,
		contract.CodeCancelled, contract.CodeRateLimited, contract.CodeDeploymentUnavailable,
		contract.CodeProviderError, contract.CodeProviderTimeout, contract.CodeProviderOverloaded,
		contract.CodeProviderCredentialInvalid, contract.CodeProviderBillingRefused,
		contract.CodeServiceUnavailable, contract.CodeInternalError,
	} {
		if got, want := classifyFailure(code), counted[code]; got != want {
			t.Errorf("classifyFailure(%s) = %d, want %d", code, got, want)
		}
	}
}

func TestAWithholdPolicyThatCouldNotReturnIsRefused(t *testing.T) {
	valid := DefaultWithholdPolicy()
	if err := valid.Validate(); err != nil {
		t.Fatalf("the default policy is refused: %v", err)
	}
	for _, check := range []struct {
		name   string
		mutate func(*WithholdPolicy)
		says   string
	}{
		{"no threshold", func(p *WithholdPolicy) { p.Failures = 0 }, "never failed"},
		{"negative span", func(p *WithholdPolicy) { p.FailureSpan = -time.Second }, "negative"},
		{"no quarantine", func(p *WithholdPolicy) { p.MinQuarantine = 0 }, "flap"},
		{"inverted clamp", func(p *WithholdPolicy) { p.MaxQuarantine = p.MinQuarantine - time.Second }, "shorter"},
		{"lookback inside the quarantine", func(p *WithholdPolicy) { p.Lookback = p.MaxQuarantine }, "must exceed"},
		{"lookback past the database bound", func(p *WithholdPolicy) { p.Lookback = 8 * 24 * time.Hour }, "exceeds"},
	} {
		t.Run(check.name, func(t *testing.T) {
			policy := DefaultWithholdPolicy()
			check.mutate(&policy)
			err := policy.Validate()
			if err == nil || !strings.Contains(err.Error(), check.says) {
				t.Fatalf("Validate() = %v, want an error saying %q", err, check.says)
			}
		})
	}
}
