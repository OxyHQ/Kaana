package publicapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

func TestIdentityVerificationBindsOnlyAuthenticationFacts(t *testing.T) {
	for _, variant := range []string{"valid", "minute precision", "wrong request", "wrong body", "wrong audience", "expired", "future", "wrong attribution", "grant domain", "unsigned", "missing internal flag", "economic field"} {
		t.Run(variant, func(t *testing.T) {
			public, private, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
			user := contract.UserID("person")
			request := contract.IdentityRequest{SchemaVersion: 1, Kind: "identity", RequestID: "identity-test", Audience: "kaana.ai", Credential: "synthetic-caller", OnBehalfOfUserID: &user}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != contract.OxyKaanaIdentitiesEndpoint {
					t.Error("identity used economic endpoint")
				}
				raw, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Error(readErr)
					return
				}
				var fields map[string]json.RawMessage
				if decodeErr := json.Unmarshal(raw, &fields); decodeErr != nil {
					t.Error(decodeErr)
					return
				}
				if len(fields) != 6 {
					t.Error("unexpected inference facts in identity request")
				}
				hash := sha256.Sum256(raw)
				assertion := contract.IdentityAssertion{SchemaVersion: 1, Kind: "identity", RequestID: request.RequestID, Audience: request.Audience, RequestSHA256: hex.EncodeToString(hash[:]), IssuedAt: contract.Timestamp(now.Add(-30 * time.Second).Format(time.RFC3339)), ExpiresAt: contract.Timestamp(now.Add(30 * time.Second).Format(time.RFC3339)), UserID: &user, Principal: contract.IdentityPrincipal{OwnerAccountID: "owner", ApplicationID: "app", CredentialID: "workload", Environment: "production", Lane: contract.IdentityLaneService, Authority: contract.AuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}}
				switch variant {
				case "minute precision":
					assertion.IssuedAt = contract.Timestamp(now.Add(-30 * time.Second).Format("2006-01-02T15:04Z"))
					assertion.ExpiresAt = contract.Timestamp(now.Add(30 * time.Second).Format("2006-01-02T15:04Z"))
				case "wrong request":
					assertion.RequestID = "other-request"
				case "wrong body":
					assertion.RequestSHA256 = hex.EncodeToString(make([]byte, 32))
				case "wrong audience":
					assertion.Audience = "elsewhere"
				case "expired":
					assertion.IssuedAt = contract.Timestamp(now.Add(-time.Minute).Format(time.RFC3339))
					assertion.ExpiresAt = contract.Timestamp(now.Format(time.RFC3339))
				case "future":
					assertion.IssuedAt = contract.Timestamp(now.Add(time.Second).Format(time.RFC3339))
					assertion.ExpiresAt = contract.Timestamp(now.Add(time.Minute).Format(time.RFC3339))
				case "wrong attribution":
					other := contract.UserID("other-person")
					assertion.UserID = &other
				}
				body, _ := json.Marshal(assertion)
				if variant == "missing internal flag" || variant == "economic field" {
					var modified map[string]any
					_ = json.Unmarshal(body, &modified)
					if variant == "missing internal flag" {
						delete(modified["principal"].(map[string]any), "applicationIsInternal")
					} else {
						modified["maxOutputTokens"] = 8192
					}
					body, _ = json.Marshal(modified)
				}
				if variant != "unsigned" {
					millis := time.Now().UnixMilli()
					input := edgeauth.IdentitySigningInput(oxyKeyID, millis, body)
					if variant == "grant domain" {
						input = edgeauth.GrantSigningInput(oxyKeyID, millis, body)
					}
					w.Header().Set(edgeauth.HeaderKeyID, oxyKeyID)
					w.Header().Set(edgeauth.HeaderTimestamp, strconv.FormatInt(millis, 10))
					w.Header().Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, input)))
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			verifier, err := publicapi.NewOxyIdentityVerifier(publicapi.OxyIdentityConfig{Oxy: oxyControl{origin: server.URL}, Keys: map[string]ed25519.PublicKey{oxyKeyID: public}, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			assertion, failure := verifier.Verify(context.Background(), request)
			valid := variant == "valid" || variant == "minute precision"
			if valid && (assertion == nil || failure != nil) {
				t.Fatalf("valid assertion refused: %v", failure)
			}
			if !valid && (assertion != nil || failure == nil) {
				t.Fatal("invalid assertion accepted")
			}
		})
	}
}

func TestIdentityVerificationCancellation(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	verifier, err := publicapi.NewOxyIdentityVerifier(publicapi.OxyIdentityConfig{Oxy: oxyControl{origin: server.URL}, Keys: map[string]ed25519.PublicKey{oxyKeyID: public}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *contract.PublicError, 1)
	go func() {
		_, failure := verifier.Verify(ctx, contract.IdentityRequest{RequestID: "cancel"})
		done <- failure
	}()
	<-started
	cancel()
	select {
	case failure := <-done:
		if failure == nil {
			t.Fatal("cancellation accepted identity")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not terminate verification")
	}
}
