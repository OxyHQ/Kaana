package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func authorityFixtures(t *testing.T) ([]fixture, []fixture) {
	t.Helper()
	principal := sampleAttribution().Principal
	principal.Authority = pointerTo(AuthorityFirstParty)
	principal.InferenceScopes = nil
	grant := GrantPrincipal{Authority: pointerTo(AuthorityFirstParty), BillingAccountID: principal.Billing.AccountID, ApplicationID: principal.ApplicationID, CredentialID: principal.CredentialID, Environment: principal.Environment, Lane: LaneServiceToken}
	valid := []fixture{
		{Schema: "authenticatedPrincipalSchema", Case: "first-party-without-scopes", Value: principal},
		{Schema: "grantPrincipalSchema", Case: "first-party-grant-without-scopes", Value: grant},
	}
	invalid := []fixture{}
	for _, entry := range valid {
		encoded, err := json.Marshal(entry.Value)
		if err != nil {
			t.Fatal(err)
		}
		field := "inferenceScopes"
		if entry.Schema == "grantPrincipalSchema" {
			field = "scopes"
		}
		for _, scope := range []any{[]string{}, nil} {
			var value map[string]any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			value[field] = scope
			invalid = append(invalid, fixture{Schema: entry.Schema, Case: entry.Case + "-scope-present", Value: value})
		}
	}
	return valid, invalid
}

func TestFirstPartyPrincipalWire(t *testing.T) {
	valid, invalid := authorityFixtures(t)
	for _, entry := range valid {
		data, err := json.Marshal(entry.Value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "Scopes") || strings.Contains(string(data), `"scopes"`) {
			t.Fatalf("own instruction carries scopes: %s", data)
		}
		if entry.Schema == "authenticatedPrincipalSchema" {
			var principal AuthenticatedPrincipal
			if err := json.Unmarshal(data, &principal); err != nil || !principal.IsFirstParty() {
				t.Fatalf("first-party roundtrip: %v", err)
			}
		} else {
			var principal GrantPrincipal
			if err := json.Unmarshal(data, &principal); err != nil || !isFirstParty(principal.Authority) {
				t.Fatalf("first-party grant roundtrip: %v", err)
			}
		}
	}
	for _, entry := range invalid {
		data, err := json.Marshal(entry.Value)
		if err != nil {
			t.Fatal(err)
		}
		var parseErr error
		if entry.Schema == "authenticatedPrincipalSchema" {
			var p AuthenticatedPrincipal
			parseErr = json.Unmarshal(data, &p)
		} else {
			var p GrantPrincipal
			parseErr = json.Unmarshal(data, &p)
		}
		if parseErr == nil {
			t.Fatalf("accepted incompatible authority: %s", data)
		}
	}
}

func TestExternalAuthorityNeverBecomesFirstParty(t *testing.T) {
	principal := sampleAttribution().Principal
	for _, authority := range []*ServiceAuthority{nil, pointerTo(AuthorityExternal)} {
		principal.Authority = authority
		if principal.IsFirstParty() {
			t.Fatal("external principal became first-party")
		}
		encoded, err := json.Marshal(principal)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AuthenticatedPrincipal
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.IsFirstParty() || len(decoded.InferenceScopes) != 2 {
			t.Fatal("external scope limits changed")
		}
	}
}
