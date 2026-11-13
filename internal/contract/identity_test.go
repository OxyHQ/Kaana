package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIdentityContract(t *testing.T) {
	value := IdentityAssertion{SchemaVersion: 1, Kind: "identity", Audience: "kaana.ai", RequestID: "req_identity", RequestSHA256: strings.Repeat("a", 64), IssuedAt: "2026-11-13T12:00:00Z", ExpiresAt: "2026-11-13T12:01:00Z", Principal: IdentityPrincipal{OwnerAccountID: "owner", ApplicationID: "app", CredentialID: "workload", Environment: EnvironmentProduction, Lane: IdentityLaneService, Authority: AuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeIdentityAssertion(raw); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		strings.Replace(string(raw), `,"applicationIsInternal":true`, "", 1),
		strings.Replace(string(raw), `"applicationIsInternal":true`, `"applicationIsInternal":null`, 1),
		strings.Replace(string(raw), `"applicationId":"app"`, `"applicationId":"`+strings.Repeat("a", 65)+`"`, 1),
		strings.Replace(string(raw), `"applicationId":"app"`, `"applicationId":"`+strings.Repeat("😀", 33)+`"`, 1),
		strings.Replace(string(raw), `"requestId":"req_identity"`, `"requestId":"`+strings.Repeat("a", 129)+`"`, 1),
		strings.Replace(string(raw), `"kind":"identity"`, `"kind":"identity","userId":null`, 1),
		strings.Replace(string(raw), `"kind":"identity"`, `"kind":"identity","userId":"`+strings.Repeat("u", 65)+`"`, 1),
		strings.Replace(string(raw), `12:00:00Z`, `12:00:00+00:00`, 1),
	} {
		if value == string(raw) {
			t.Fatal("required-field mutation did not apply")
		}
		if _, err := DecodeIdentityAssertion([]byte(value)); err == nil {
			t.Fatal("accepted malformed identity field")
		}
	}
	bounded := strings.Replace(string(raw), `"applicationId":"app"`, `"applicationId":"`+strings.Repeat("😀", 32)+`"`, 1)
	if _, err := DecodeIdentityAssertion([]byte(bounded)); err != nil {
		t.Fatalf("UTF-16 positive boundary: %v", err)
	}
	for _, oldnew := range [][2]string{{`"expiresAt":"2026-11-13T12:01:00Z"`, `"expiresAt":"2026-11-13T12:01:01Z"`}, {`"lane":"service_token"`, `"lane":"machine_credential"`}, {`"lane":"service_token"`, `"lane":"product_session"`}, {`"authority":"first_party"`, `"authority":"external"`}, {`"applicationIsInternal":true`, `"applicationIsInternal":true,"scopes":[]`}, {`"kind":"identity"`, `"kind":"identity","economics":{}`}} {
		changed := strings.Replace(string(raw), oldnew[0], oldnew[1], 1)
		if changed == string(raw) {
			t.Fatalf("mutation did not apply: %s", oldnew[0])
		}
		if _, err := DecodeIdentityAssertion([]byte(changed)); err == nil {
			t.Fatalf("accepted mutation %s", oldnew[1])
		}
	}
	value.Principal.Authority = AuthorityExternal
	value.Principal.Lane = IdentityLaneMachine
	raw, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"scopes":[]`) {
		t.Fatal("external principal lost explicit scopes")
	}
	if _, err := DecodeIdentityAssertion(raw); err != nil {
		t.Fatal(err)
	}
}
