package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type identityCheckVerifier func(context.Context, contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError)

func (f identityCheckVerifier) Verify(ctx context.Context, request contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError) {
	return f(ctx, request)
}

func TestIdentityCheckChecksOwnWorkloadWithoutDatabaseOrInference(t *testing.T) {
	var output bytes.Buffer
	closed := false
	open := func(ctx context.Context, getenv func(string) string) (identityCheckClient, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded canary")
		}
		return identityCheckClient{
			mint: func(context.Context) (string, error) { return "ephemeral-test-credential", nil },
			verify: identityCheckVerifier(func(_ context.Context, request contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError) {
				if request.Kind != "identity" || request.SchemaVersion != 1 || request.Audience != "kaana.ai" || request.OnBehalfOfUserID != nil || request.Credential != "ephemeral-test-credential" || !strings.HasPrefix(string(request.RequestID), "identity-check-") {
					t.Fatal("identity-only request changed")
				}
				return &contract.IdentityAssertion{Principal: contract.IdentityPrincipal{ApplicationID: "kaana-test", Authority: contract.IdentityAuthorityFirstParty, Lane: contract.IdentityLaneService, Environment: contract.EnvironmentProduction}}, nil
			}),
			close: func(context.Context) error { closed = true; return nil },
		}, nil
	}
	if err := runIdentityCheck([]string{"--application-id", "kaana-test"}, &output, func(key string) string {
		t.Fatalf("unexpected environment read: %s", key)
		return ""
	}, open); err != nil {
		t.Fatal(err)
	}
	if !closed || output.String() != "{\"verified\":true}\n" {
		t.Fatalf("unexpected canary outcome: closed=%v output=%q", closed, output.String())
	}
}

func TestIdentityCheckRefusesWrongIdentityAndSanitizesFailures(t *testing.T) {
	for _, scenario := range []string{"foreign-app", "external", "session", "staging", "attribution", "scopes", "refusal", "mint", "open", "close"} {
		t.Run(scenario, func(t *testing.T) {
			var output bytes.Buffer
			closed := false
			open := func(context.Context, func(string) string) (identityCheckClient, error) {
				if scenario == "open" {
					return identityCheckClient{}, errors.New("private diagnostic")
				}
				return identityCheckClient{
					mint: func(context.Context) (string, error) {
						if scenario == "mint" {
							return "", errors.New("private diagnostic")
						}
						return "ephemeral-test-credential", nil
					},
					verify: identityCheckVerifier(func(_ context.Context, request contract.IdentityRequest) (*contract.IdentityAssertion, *contract.PublicError) {
						assertion := &contract.IdentityAssertion{Principal: contract.IdentityPrincipal{ApplicationID: "kaana-test", Authority: contract.IdentityAuthorityFirstParty, Lane: contract.IdentityLaneService, Environment: contract.EnvironmentProduction}}
						switch scenario {
						case "foreign-app":
							assertion.Principal.ApplicationID = "other-app"
						case "external":
							assertion.Principal.Authority = contract.IdentityAuthorityExternal
						case "session":
							assertion.Principal.Lane = contract.IdentityLaneSession
						case "staging":
							assertion.Principal.Environment = contract.EnvironmentStaging
						case "attribution":
							user := contract.UserID("person")
							assertion.UserID = &user
						case "scopes":
							assertion.Principal.Scopes = []contract.Scope{"inference:read"}
						case "refusal":
							return nil, contract.NewPublicError(request.RequestID, "service_unavailable", "private diagnostic")
						}
						return assertion, nil
					}),
					close: func(context.Context) error {
						closed = true
						if scenario == "close" {
							return errors.New("private diagnostic")
						}
						return nil
					},
				}, nil
			}
			err := runIdentityCheck([]string{"--application-id", "kaana-test"}, &output, func(string) string { return "" }, open)
			if err == nil || strings.Contains(err.Error(), "private diagnostic") || output.Len() != 0 || (scenario != "open" && !closed) {
				t.Fatalf("unsafe outcome: err=%v output=%q closed=%v", err, output.String(), closed)
			}
		})
	}
}

func TestIdentityCheckRejectsConfigurationBeforeWorkloadMint(t *testing.T) {
	for _, environment := range []map[string]string{{}, {"KAANA_OXY_API_BASE_URL": "https://other.invalid", "KAANA_OXY_SERVICE_ENVIRONMENT": "production"}, {"KAANA_OXY_API_BASE_URL": "https://api.oxy.so", "KAANA_OXY_SERVICE_ENVIRONMENT": "staging"}, {"KAANA_OXY_API_BASE_URL": "https://api.oxy.so", "KAANA_OXY_SERVICE_ENVIRONMENT": "production", "KAANA_EDGE_PUBLIC_KEYS": "invalid"}} {
		if _, err := openIdentityCheck(context.Background(), func(key string) string { return environment[key] }); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	var output bytes.Buffer
	if err := run([]string{"identity-check"}, strings.NewReader(""), &output, func(key string) string {
		t.Fatalf("missing arguments reached configuration: %s", key)
		return ""
	}); err == nil || output.Len() != 0 {
		t.Fatal("missing application identity was accepted")
	}
}
