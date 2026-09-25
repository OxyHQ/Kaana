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
	mu       sync.Mutex
	calls    int
	origins  []string
	token    string
	lifetime time.Duration
	err      error
}

func (a *attestedIdentity) Mint(_ context.Context, origin *url.URL) (string, time.Duration, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
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

func TestNewDecidesTheThreeIdentityStates(t *testing.T) {
	cases := map[string]struct {
		apiKey    string
		apiSecret string
		attested  bool
		wantError string
	}{
		"an attestable role and a key pair is the safe resting state": {
			apiKey: "oxy_dk_kaana", apiSecret: "kaana-service-secret", attested: true,
		},
		"a key pair alone still works": {
			apiKey: "oxy_dk_kaana", apiSecret: "kaana-service-secret",
		},
		"an attestable role alone needs no secret": {
			attested: true,
		},
		"neither is the only state that is refused": {
			wantError: "no Oxy identity",
		},
		"half a pair is a typo, not a state": {
			apiKey:    "oxy_dk_kaana",
			wantError: "needs both halves",
		},
		"half a pair is a typo even when this process can attest": {
			apiSecret: "kaana-service-secret", attested: true,
			wantError: "needs both halves",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			config := Config{
				BaseURL: "http://127.0.0.1:1", APIKey: testCase.apiKey, APISecret: testCase.apiSecret,
				Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
			}
			if testCase.attested {
				config.WorkloadMinter = &attestedIdentity{}
			}
			reporter, err := New(config)
			if testCase.wantError == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := reporter.Close(closeContext); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New succeeded; want %q", testCase.wantError)
			}
			if !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.wantError)
			}
		})
	}
}

// oxyForIdentity serves the token endpoints and one verdict path, recording
// which identity each token came from.
type oxyForIdentity struct {
	server *httptest.Server

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

func TestReporterPrefersTheKeyPairAndCachesOneToken(t *testing.T) {
	fake := newOxyForIdentity(t)
	attested := &attestedIdentity{}
	reporter, err := New(Config{
		BaseURL: fake.server.URL, APIKey: "oxy_dk_kaana", APISecret: "kaana-service-secret",
		WorkloadMinter: attested, Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(verdictFor(7))
	reporter.Submit(verdictFor(8))
	drain(t, reporter)

	mints, _ := attested.observed()
	pairMints, authorization := fake.observed()
	if pairMints != 1 {
		t.Fatalf("key-pair mints = %d; a short-lived token is cached, not minted per verdict", pairMints)
	}
	if mints != 0 {
		t.Fatalf("the workload identity was used %d times although the pair works: an attested token cannot report a verdict yet", mints)
	}
	for _, header := range authorization {
		if header != "Bearer key-pair-service-token" {
			t.Fatalf("authorization = %q", header)
		}
	}
}

func TestReporterFallsBackToTheAttestedIdentityWhenTheKeyPairFails(t *testing.T) {
	fake := newOxyForIdentity(t)
	fake.pairStatus = http.StatusUnauthorized
	attested := &attestedIdentity{}
	reporter, err := New(Config{
		BaseURL: fake.server.URL, APIKey: "oxy_dk_kaana", APISecret: "kaana-service-secret",
		WorkloadMinter: attested, Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reporter.Submit(verdictFor(7))
	drain(t, reporter)

	mints, origins := attested.observed()
	pairMints, authorization := fake.observed()
	if pairMints != 1 || mints != 1 {
		t.Fatalf("key-pair mints = %d, attestations = %d; want one of each", pairMints, mints)
	}
	if len(authorization) != 1 || authorization[0] != "Bearer attested-service-token" {
		t.Fatalf("authorization = %v; the verdict must still be delivered", authorization)
	}
	// The origin is the reporter's own, validated one. An attestation sent
	// anywhere else can be relayed into a Kaana token.
	if len(origins) != 1 || origins[0] != fake.server.URL {
		t.Fatalf("attested against %v, want %q", origins, fake.server.URL)
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
