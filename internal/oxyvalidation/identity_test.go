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
