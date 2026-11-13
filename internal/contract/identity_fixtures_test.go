package contract

import "strings"

func identityWireFixtures() ([]fixture, []fixture) {
	owner := AccountID("application-owner")
	user := UserID("person-account")
	binding := IdentityApplicationBinding("public_client_id")
	value := IdentityAssertion{SchemaVersion: 1, Kind: "identity", Audience: "kaana.ai", RequestID: "req-identity-wire", RequestSHA256: strings.Repeat("a", 64), IssuedAt: "2026-11-13T12:00Z", ExpiresAt: "2026-11-13T12:01Z", Principal: IdentityPrincipal{OwnerAccountID: &owner, ApplicationID: "application", CredentialID: "workload", Environment: EnvironmentProduction, Lane: IdentityLaneService, Authority: IdentityAuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}}
	valid := []fixture{{Schema: "kaanaIdentityAssertionSchema", Case: "workload identity", Value: value}}
	value.Principal.Authority = IdentityAuthorityExternal
	value.Principal.Lane = IdentityLaneMachine
	// Empty scopes remain an explicit external identity without invented authority.
	value.Principal.Scopes = []Scope{}
	valid = append(valid, fixture{Schema: "kaanaIdentityAssertionSchema", Case: "external identity", Value: value})
	value.Principal = IdentityPrincipal{ApplicationID: "application", CredentialID: "session", Environment: EnvironmentProduction, Lane: IdentityLaneSession, Authority: IdentityAuthoritySession, ApplicationClass: "first_party", ApplicationIsInternal: true, ApplicationOwnerAccountID: &owner, VerifiedRequester: &IdentityVerifiedRequester{SubjectAccountID: "person-account", PrincipalUserID: "person-user"}, ApplicationBinding: &binding}
	value.UserID = &user
	valid = append(valid, fixture{Schema: "kaanaIdentityAssertionSchema", Case: "session identity", Value: value})
	invalid := []fixture{}
	missing := value
	missing.UserID = nil
	invalid = append(invalid, fixture{Schema: "kaanaIdentityAssertionSchema", Case: "session missing subject", Value: missing})
	wrong := value
	wrongUser := UserID("application-owner")
	wrong.UserID = &wrongUser
	invalid = append(invalid, fixture{Schema: "kaanaIdentityAssertionSchema", Case: "session app owner is not subject", Value: wrong})
	mixed := value
	mixed.Principal.OwnerAccountID = &owner
	invalid = append(invalid, fixture{Schema: "kaanaIdentityAssertionSchema", Case: "session mixed workload authority", Value: mixed})
	lane := value
	lane.Principal.Lane = IdentityLaneService
	invalid = append(invalid, fixture{Schema: "kaanaIdentityAssertionSchema", Case: "session mixed workload lane", Value: lane})
	return valid, invalid
}
