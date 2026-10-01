package provider

import (
	"context"
	"errors"
	"github.com/OxyHQ/Kaana/internal/contract"
	"testing"
)

func TestScopedCredentialAttemptClaimsBeforeOneExactExchange(t *testing.T) {
	for name, result := range map[string]CredentialedAttempt{
		"accepted":  {Accepted: true},
		"rejected":  {Failure: upstreamFailure(contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication)},
		"billing":   {Failure: upstreamFailure(contract.CodeProviderBillingRefused, contract.UpstreamQuota)},
		"throttle":  {Failure: upstreamFailure(contract.CodeRateLimited, contract.UpstreamRateLimit)},
		"uncertain": {Transport: true, Failure: errors.New("synthetic disconnect")},
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := NewKeyPool("test-provider", []KeyDeclaration{{KeyID: "exact", Secret: firstCredential}, {KeyID: "other", Secret: secondCredential}}, KeyPolicy{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := pool.Bind("exact")
			if err != nil {
				t.Fatal(err)
			}
			claimed := false
			claims, sends := 0, 0
			call := &Call{ScopedAttempt: &ScopedCredentialAttempt{KeyID: "exact", Claim: func(context.Context) error {
				claims++
				if claimed {
					return errors.New("already claimed")
				}
				claimed = true
				return nil
			}}}
			exchange := func(_ context.Context, key Key) (int, CredentialedAttempt) {
				sends++
				if !claimed || key.ID != "exact" {
					t.Fatal("sent without exact prior claim")
				}
				return 1, result
			}
			_, _, _ = WalkAttempts(context.Background(), bound, call, exchange)
			if sends != 1 || claims != 1 {
				t.Fatalf("sends=%d claims=%d", sends, claims)
			}
			_, _, _ = WalkAttempts(context.Background(), bound, call, exchange)
			if sends != 1 {
				t.Fatal("repeated provider work")
			}
		})
	}
}

func TestScopedCredentialAttemptRejectsUnboundPoolAndFailedClaim(t *testing.T) {
	pool, err := NewKeyPool("test-provider", []KeyDeclaration{{KeyID: "exact", Secret: firstCredential}, {KeyID: "other", Secret: secondCredential}}, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sends, claims := 0, 0
	call := &Call{ScopedAttempt: &ScopedCredentialAttempt{KeyID: "exact", Claim: func(context.Context) error { claims++; return errors.New("database uncertain") }}}
	exchange := func(context.Context, Key) (int, CredentialedAttempt) {
		sends++
		return 1, CredentialedAttempt{Accepted: true}
	}
	if _, _, err = WalkAttempts(context.Background(), pool, call, exchange); err == nil {
		t.Fatal("unbound pool accepted")
	}
	if claims != 0 || sends != 0 {
		t.Fatal("unbound pool claimed or sent")
	}
	bound, err := pool.Bind("exact")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = WalkAttempts(context.Background(), bound, call, exchange); err == nil {
		t.Fatal("uncertain claim accepted")
	}
	if claims != 1 || sends != 0 {
		t.Fatal("failed claim sent")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _ = WalkAttempts(canceled, bound, call, exchange)
	if claims != 1 || sends != 0 {
		t.Fatal("canceled request attempted")
	}
}
