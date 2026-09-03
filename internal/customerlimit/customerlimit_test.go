package customerlimit

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

func TestThrottleAndBreakerAreExactToCustomerGeneration(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	registry := NewRegistry(func() time.Time { return now })
	scope := customerScope("acc_one", "conn_one", 3)
	permit, refusal := registry.Admit(scope)
	if permit == nil || refusal.Reason != ReasonNone {
		t.Fatalf("initial admit = %#v/%#v", permit, refusal)
	}
	permit.Throttled(12 * time.Second)
	if permit, refusal = registry.Admit(scope); permit != nil || refusal.Reason != ReasonThrottled || refusal.RetryAfter != 12*time.Second {
		t.Fatalf("throttled admit = %#v/%#v", permit, refusal)
	}
	otherAccount := scope
	otherAccount.OwnerAccountID = "acc_two"
	if permit, refusal = registry.Admit(otherAccount); permit == nil || refusal.Reason != ReasonNone {
		t.Fatalf("another account inherited throttle = %#v/%#v", permit, refusal)
	}
	otherRevision := scope
	otherRevision.Revision++
	if permit, refusal = registry.Admit(otherRevision); permit == nil || refusal.Reason != ReasonNone {
		t.Fatalf("another revision inherited throttle = %#v/%#v", permit, refusal)
	}
	now = now.Add(12 * time.Second)
	trial, refusal := registry.Admit(scope)
	if trial == nil || refusal.Reason != ReasonNone {
		t.Fatalf("half-open trial = %#v/%#v", trial, refusal)
	}
	if duplicate, blocked := registry.Admit(scope); duplicate != nil || blocked.Reason != ReasonThrottled {
		t.Fatalf("parallel half-open admit = %#v/%#v", duplicate, blocked)
	}
	trial.Succeeded()
	if permit, refusal = registry.Admit(scope); permit == nil || refusal.Reason != ReasonNone {
		t.Fatalf("admit after success = %#v/%#v", permit, refusal)
	}
}

func TestRejectedGenerationDoesNotContaminateAnotherConnection(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	registry := NewRegistry(func() time.Time { return now })
	rejected := customerScope("acc_one", "conn_one", 1)
	permit, _ := registry.Admit(rejected)
	permit.Rejected()
	if permit, refusal := registry.Admit(rejected); permit != nil || refusal.Reason != ReasonRejected || refusal.RetryAfter <= 0 {
		t.Fatalf("rejected admit = %#v/%#v", permit, refusal)
	}
	healthy := rejected
	healthy.ConnectionID = "conn_two"
	if permit, refusal := registry.Admit(healthy); permit == nil || refusal.Reason != ReasonNone {
		t.Fatalf("healthy connection inherited rejection = %#v/%#v", permit, refusal)
	}
}

func customerScope(owner, connection string, revision int64) credentialstore.CustomerCredentialScope {
	return credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
			Provider: "anthropic", OwnerAccountID: owner, ConnectionID: connection,
			Environment: contract.EnvironmentProduction,
		},
		CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", Revision: revision,
	}
}
