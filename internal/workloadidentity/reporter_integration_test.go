package workloadidentity

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
)

// Exercise the real producer, reporter and HTTP callback together. The fake
// authority re-derives the STS signature rather than accepting a named identity.
func TestAttestedProducerReportsExactCallbackAndReprovesAfterRefusal(t *testing.T) {
	authority := newOxy(t)
	callbacks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/validation") {
			callbacks++
			if r.Header.Get("Authorization") != "Bearer oxy-service-token-from-attestation" {
				t.Error("callback did not use the attested workload token")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"credentialHandle":"kcred_abcdefghijklmnopqrstuvwxyz","credentialRevision":7,"state":"valid"}` {
				t.Errorf("callback selector changed: %s", body)
			}
			if callbacks == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		authority.handle(w, r)
	}))
	defer server.Close()
	reporter, err := oxyvalidation.New(oxyvalidation.Config{
		BaseURL: server.URL, WorkloadMinter: newTestClient(t, taskRoleCredentials()),
		Environment: contract.EnvironmentDevelopment, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() { _ = reporter.Close(ctx) }()
	// Control calls and callbacks retain independent audience-specific mints.
	if _, err := reporter.ControlToken(ctx); err != nil {
		t.Fatal(err)
	}
	reporter.Submit(oxyvalidation.Verdict{
		ConnectionID: "conn_exact", CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz",
		CredentialRevision: 7, Environment: contract.EnvironmentDevelopment, State: oxyvalidation.StateValid,
	})
	if err := reporter.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if callbacks != 2 || authority.exchanges != 3 {
		t.Fatalf("callbacks=%d exchanges=%d; want 2 callbacks, 3 signed exchanges", callbacks, authority.exchanges)
	}
}
