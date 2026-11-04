package oxyvalidation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// attestedIdentity is a workload identity that mints without a secret, standing
// in for internal/workloadidentity.
type attestedIdentity struct {
	mu        sync.Mutex
	calls     int
	origins   []string
	audiences []contract.OxyAudience
	token     string
	lifetime  time.Duration
	err       error
}

func (a *attestedIdentity) Mint(_ context.Context, origin *url.URL, audience contract.OxyAudience) (string, time.Duration, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.audiences = append(a.audiences, audience)
	if origin != nil {
		a.origins = append(a.origins, origin.String())
	}
	if a.err != nil {
		return "", 0, a.err
	}
	token, lifetime := a.token, a.lifetime
	if token == "" {
		token = "attested-service-token"
	}
	if audience != contract.OxyAPIAudience {
		token += "@" + string(audience)
	}
	if lifetime == 0 {
		lifetime = time.Hour
	}
	return token, lifetime, nil
}

func (a *attestedIdentity) observed() (int, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, append([]string(nil), a.origins...)
}

func TestNewRequiresWorkloadIdentity(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://api.oxy.so", Environment: contract.EnvironmentProduction}); err == nil {
		t.Fatal("own reporter accepted absent workload identity")
	}
}

// oxyForIdentity detects any accidental use of the removed key endpoint.
type oxyForIdentity struct {
	server        *httptest.Server
	mu            sync.Mutex
	pairMints     int
	authorization []string
	verdictStatus int
	pairStatus    int
}

func newOxyForIdentity(t *testing.T) *oxyForIdentity {
	t.Helper()
	fake := &oxyForIdentity{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.URL.Path == "/auth/service-token":
			fake.pairMints++
			if fake.pairStatus != 0 {
				w.WriteHeader(fake.pairStatus)
				_, _ = io.WriteString(w, `{"error":"credential_revoked"}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"token":"key-pair-service-token","expiresIn":3600}}`)
		case strings.HasSuffix(r.URL.Path, "/validation"):
			fake.authorization = append(fake.authorization, r.Header.Get("Authorization"))
			status := fake.verdictStatus
			if status == 0 || len(fake.authorization) > 1 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"data":{"id":"conn_customer_01"}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (o *oxyForIdentity) observed() (int, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pairMints, append([]string(nil), o.authorization...)
}

func verdictFor(revision int64) Verdict {
	return Verdict{
		ConnectionID: "conn_customer_01", CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz",
		CredentialRevision: revision, Environment: contract.EnvironmentDevelopment,
		State: StateInvalid, FailureCode: FailureUnauthorized,
	}
}

func drain(t *testing.T, reporter *Reporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := reporter.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestReporterUsesAndCachesWorkloadIdentity(t *testing.T) {
	fake := newOxyForIdentity(t)
	identity := &attestedIdentity{}
	reporter, err := New(Config{BaseURL: fake.server.URL, WorkloadMinter: identity,
		Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	reporter.Submit(verdictFor(7))
	reporter.Submit(verdictFor(8))
	drain(t, reporter)
	mints, origins := identity.observed()
	pairMints, authorization := fake.observed()
	if mints != 1 || pairMints != 0 || len(authorization) != 2 || len(origins) != 1 || origins[0] != fake.server.URL {
		t.Fatalf("workload mints=%d key mints=%d requests=%v origins=%v", mints, pairMints, authorization, origins)
	}
	for _, header := range authorization {
		if header != "Bearer attested-service-token" {
			t.Fatal("wrong callback identity")
		}
	}
}

func TestReporterWithNoKeyPairPropagatesTheAttestationFailure(t *testing.T) {
	fake := newOxyForIdentity(t)
	attested := &attestedIdentity{err: errors.New("workload identity: Oxy refused the attestation: HTTP 403")}
	reporter, err := New(Config{
		BaseURL: fake.server.URL, WorkloadMinter: attested,
		Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(verdictFor(7))
	drain(t, reporter)

	pairMints, authorization := fake.observed()
	if pairMints != 0 {
		t.Fatalf("a process with no key pair minted from one %d times", pairMints)
	}
	if len(authorization) != 0 {
		t.Fatalf("a verdict went out unauthenticated: %v", authorization)
	}
}

func TestAttestedTokenIsRemintedAfterOxyRefusesIt(t *testing.T) {
	fake := newOxyForIdentity(t)
	fake.verdictStatus = http.StatusUnauthorized
	attested := &attestedIdentity{}
	reporter, err := New(Config{
		BaseURL: fake.server.URL, WorkloadMinter: attested,
		Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(verdictFor(7))
	drain(t, reporter)

	mints, _ := attested.observed()
	_, authorization := fake.observed()
	if mints != 2 {
		t.Fatalf("attestations = %d; a token Oxy no longer accepts is re-proved once", mints)
	}
	if len(authorization) != 2 {
		t.Fatalf("verdict attempts = %d, want two", len(authorization))
	}
}

func TestAttestedTokenIsNotServedPastItsLifetime(t *testing.T) {
	fake := newOxyForIdentity(t)
	// Shorter than the one-minute drift margin: a token this close to expiry is
	// already unusable, so the next verdict must prove the identity again.
	attested := &attestedIdentity{lifetime: 30 * time.Second}
	reporter, err := New(Config{
		BaseURL: fake.server.URL, WorkloadMinter: attested,
		Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(verdictFor(7))
	reporter.Submit(verdictFor(8))
	drain(t, reporter)

	if mints, _ := attested.observed(); mints != 2 {
		t.Fatalf("attestations = %d, want one per verdict for a token inside the drift margin", mints)
	}
}

// The public API's grant and settlement calls need Oxy's control audience, and
// Oxy refuses an oxy-api token there (401 kaana_workload_required). Each
// audience is minted and cached on its own, for the same workload identity.
func TestControlTokenIsMintedForTheKaanaControlAudience(t *testing.T) {
	t.Run("from the attested workload", func(t *testing.T) {
		identity := &attestedIdentity{}
		reporter, err := New(Config{
			BaseURL: "https://api.oxy.so", WorkloadMinter: identity, Environment: contract.EnvironmentProduction,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = reporter.Close(context.Background()) }()
		for range 2 {
			control, err := reporter.ControlToken(context.Background())
			if err != nil || control != "attested-service-token@oxy-api:kaana-control" {
				t.Fatalf("ControlToken = %q, %v", control, err)
			}
			api, err := reporter.ServiceToken(context.Background())
			if err != nil || api != "attested-service-token" {
				t.Fatalf("ServiceToken = %q, %v", api, err)
			}
		}
		identity.mu.Lock()
		defer identity.mu.Unlock()
		want := []contract.OxyAudience{contract.OxyKaanaControlAudience, contract.OxyAPIAudience}
		if len(identity.audiences) != len(want) || identity.audiences[0] != want[0] || identity.audiences[1] != want[1] {
			t.Fatalf("minted audiences = %v, want one of each, cached: %v", identity.audiences, want)
		}
	})

}

// The control token's destination is derived, never configured: each control
// route is its fixed path on the origin New validated, the one the token is
// minted by. A base URL that would put it anywhere else never builds a reporter
// (TestOnlyTheCanonicalOxyOriginBuildsAReporter), so there is no URL for it to
// reach.
func TestControlURLIsTheFixedRouteOnTheValidatedOrigin(t *testing.T) {
	for _, tc := range []struct {
		name           string
		baseURL        string
		environment    contract.Environment
		authorizations string
		settlements    string
	}{
		{"production", "https://api.oxy.so", contract.EnvironmentProduction,
			"https://api.oxy.so/internal/kaana/v1/authorizations", "https://api.oxy.so/internal/kaana/v1/settlements"},
		{"development on loopback", "http://127.0.0.1:4100", contract.EnvironmentDevelopment,
			"http://127.0.0.1:4100/internal/kaana/v1/authorizations", "http://127.0.0.1:4100/internal/kaana/v1/settlements"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reporter, err := New(Config{
				BaseURL: tc.baseURL, WorkloadMinter: &attestedIdentity{}, Environment: tc.environment,
				Logger: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer func() { _ = reporter.Close(context.Background()) }()
			if got := reporter.ControlURL(contract.OxyKaanaAuthorizationsEndpoint); got != tc.authorizations {
				t.Errorf("authorizations = %q, want %q", got, tc.authorizations)
			}
			if got := reporter.ControlURL(contract.OxyKaanaSettlementsEndpoint); got != tc.settlements {
				t.Errorf("settlements = %q, want %q", got, tc.settlements)
			}
		})
	}
}

// Every Oxy credential Kaana holds — the BYOK callback's token and the control
// token the public API sends with each grant and settlement — goes only to the
// origin this accepts. Production and staging accept exactly
// https://api.oxy.so; development, an explicit loopback origin and nothing else.
func TestOnlyTheCanonicalOxyOriginBuildsAReporter(t *testing.T) {
	for _, tc := range []struct {
		baseURL     string
		environment contract.Environment
		accepted    bool
	}{
		{"https://api.oxy.so", contract.EnvironmentProduction, true},
		{"https://api.oxy.so", contract.EnvironmentStaging, true},
		{"https://api.oxy.so", contract.EnvironmentDevelopment, true},
		{"http://localhost:4100", contract.EnvironmentDevelopment, true},
		{"https://127.0.0.1:4100", contract.EnvironmentDevelopment, true},
		{"http://[::1]:4100", contract.EnvironmentDevelopment, true},

		{"", contract.EnvironmentProduction, false},
		{"http://api.oxy.so", contract.EnvironmentProduction, false},
		{"https://api.oxy.so:8443", contract.EnvironmentProduction, false},
		{"https://api.oxy.so/", contract.EnvironmentProduction, false},
		{"https://api.oxy.so/internal/kaana/v1/authorizations", contract.EnvironmentProduction, false},
		{"https://api.oxy.so?next=https://attacker.example", contract.EnvironmentProduction, false},
		{"https://api.oxy.so#fragment", contract.EnvironmentProduction, false},
		{"https://kaana@api.oxy.so", contract.EnvironmentProduction, false},
		{"https://api.oxy.so.attacker.example", contract.EnvironmentProduction, false},
		{"https://attacker.example", contract.EnvironmentProduction, false},
		{"https://attacker.example", contract.EnvironmentStaging, false},
		{"http://127.0.0.1:4100", contract.EnvironmentProduction, false},
		{" https://api.oxy.so", contract.EnvironmentProduction, false},
		{"https://attacker.example", contract.EnvironmentDevelopment, false},
		{"http://10.0.0.5:4100", contract.EnvironmentDevelopment, false},
		{"ftp://127.0.0.1", contract.EnvironmentDevelopment, false},
		{"http://user@127.0.0.1:4100", contract.EnvironmentDevelopment, false},
	} {
		t.Run(string(tc.environment)+" "+tc.baseURL, func(t *testing.T) {
			reporter, err := New(Config{
				BaseURL: tc.baseURL, WorkloadMinter: &attestedIdentity{}, Environment: tc.environment,
				Logger: slog.New(slog.DiscardHandler),
			})
			if reporter != nil {
				defer func() { _ = reporter.Close(context.Background()) }()
			}
			if accepted := err == nil; accepted != tc.accepted {
				t.Fatalf("accepted = %v (%v), want %v", accepted, err, tc.accepted)
			}
		})
	}
}
