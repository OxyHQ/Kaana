package publicapi_test

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"testing"
)

func TestNativeApplicationAuthorityNeverFallsBackForUnenrolledActors(t *testing.T) {
	for _, actor := range []string{"approved", "unknown-handle", "other-owner", "machine", "session"} {
		for _, path := range []string{"/v1/responses", "/v1/models"} {
			t.Run(actor+path, func(t *testing.T) {
				config, _, catalogue := nativeFixture(t)
				p := &config.Identity.(*nativeIdentity).principal
				switch actor {
				case "unknown-handle":
					p.CredentialID = "other-workload"
				case "other-owner":
					p.OwnerAccountID = nativePtr(contract.AccountID("other"))
				case "machine":
					p.Lane = contract.IdentityLaneMachine
					p.Authority = contract.IdentityAuthorityExternal
				case "session":
					p.Lane = contract.IdentityLaneSession
					p.Authority = contract.IdentityAuthoritySession
					p.OwnerAccountID = nil
				}
				h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
				status := 0
				if path == "/v1/models" {
					status = h.get(path, validKey).StatusCode
				} else {
					status = h.post(path, validKey, map[string]any{"model": "auto", "input": "hello", "stream": true}).StatusCode
				}
				if actor == "approved" {
					if status != 200 {
						t.Fatal("approved positive control", status)
					}
				} else if status == 200 || h.adapter.called() != 0 {
					t.Fatal("unapproved actor executed", status)
				}
				if calls := h.oxy.authorizationBodies(); len(calls) != 0 {
					t.Fatal("selected application escaped to Oxy authority", len(calls))
				}
			})
		}
	}
}

func TestNativeOtherApplicationOrEnvironmentRetainsLegacyAuthority(t *testing.T) {
	for _, scope := range []string{"application", "environment"} {
		t.Run(scope, func(t *testing.T) {
			config, _, catalogue := nativeFixture(t)
			p := &config.Identity.(*nativeIdentity).principal
			if scope == "application" {
				p.ApplicationID = "unmigrated"
			} else {
				p.Environment = contract.EnvironmentStaging
			}
			h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
			response := h.get("/v1/models", validKey)
			if response.StatusCode != 200 || len(h.oxy.authorizationBodies()) != 1 {
				t.Fatal("unmigrated positive control did not use original authority", response.StatusCode)
			}
		})
	}
}
