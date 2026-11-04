package publicapi_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

// recorder counts what reaches a server and with which credential.
type recorder struct {
	mu       sync.Mutex
	requests int
	bearers  []string
}

func (r *recorder) record(request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	r.bearers = append(r.bearers, request.Header.Get("Authorization"))
}

func (r *recorder) seen() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests, append([]string(nil), r.bearers...)
}

// A redirect from Oxy's origin is an answer, never a destination: neither the
// grant request nor a settlement follows one, so the control token, the
// request and Kaana's settlement signature reach Oxy's origin and nothing
// else — not another origin, and not another path on the same one.
func TestTheControlTokenNeverFollowsARedirect(t *testing.T) {
	calls := map[string]func(t *testing.T, oxy publicapi.OxyControl) bool{
		"authorization": authorizeOnce,
		"settlement":    settleOnce,
	}
	for name, call := range calls {
		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			for _, target := range []string{"another origin", "another path on Oxy"} {
				t.Run(name+"/"+http.StatusText(status)+"/"+target, func(t *testing.T) {
					elsewhere := &recorder{}
					attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						elsewhere.record(r)
						w.WriteHeader(http.StatusOK)
					}))
					defer attacker.Close()
					oxy := &recorder{}
					mux := http.NewServeMux()
					var location string
					mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
						elsewhere.record(r)
						w.WriteHeader(http.StatusOK)
					})
					mux.HandleFunc("/internal/kaana/v1/", func(w http.ResponseWriter, r *http.Request) {
						oxy.record(r)
						http.Redirect(w, r, location, status)
					})
					server := httptest.NewServer(mux)
					defer server.Close()
					location = attacker.URL + "/collect"
					if target == "another path on Oxy" {
						location = server.URL + "/elsewhere"
					}

					if call(t, oxyControl{origin: server.URL}) {
						t.Fatal("a redirect was taken as Oxy's answer")
					}
					// Positive control: the call did reach Oxy, with the token.
					if requests, bearers := oxy.seen(); requests != 1 || bearers[0] != "Bearer "+kaanaToken {
						t.Fatalf("Oxy saw %d requests with %v", requests, bearers)
					}
					if requests, bearers := elsewhere.seen(); requests != 0 {
						t.Fatalf("the redirect was followed: %d requests, credentials %v", requests, bearers)
					}
				})
			}
		}
	}
}

// authorizeOnce asks for one grant and reports whether one was granted.
func authorizeOnce(t *testing.T, oxy publicapi.OxyControl) bool {
	t.Helper()
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := edgeauth.NewGrantVerifier(map[string]ed25519.PublicKey{oxyKeyID: public}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := publicapi.NewOxyAuthorizer(publicapi.OxyAuthorizerConfig{
		Oxy: oxy, Grants: grants, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, failure := authorizer.Authorize(context.Background(), contract.InferenceAuthorizationRequest{RequestID: "req_redirect"})
	return grant != nil && failure == nil
}

// settleOnce delivers one settlement and reports whether it was acknowledged.
func settleOnce(t *testing.T, oxy publicapi.OxyControl) bool {
	t.Helper()
	var seed [ed25519.SeedSize]byte
	signer, err := edgeauth.NewSettlementSigner("kaana-settlement-test:" + base64.StdEncoding.EncodeToString(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	outbox := &memoryOutbox{}
	if err := outbox.RecordSettlement(context.Background(), nil, credentialstore.OutboxSettlement{
		SettlementID: "set_redirect", GrantID: "grant_redirect", RequestID: "req_redirect", Body: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	settler, err := publicapi.NewSettler(publicapi.SettlerConfig{
		Store: outbox, Oxy: oxy, Signer: signer, Logger: slog.New(slog.DiscardHandler), Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	settler.DeliverDue(context.Background())
	rows := outbox.snapshot()
	return len(rows) == 1 && rows[0].delivered != ""
}
