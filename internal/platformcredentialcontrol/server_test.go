package platformcredentialcontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

type writerFake struct{ calls int }

func (w *writerFake) Import(_ context.Context, mutation Mutation, secret []byte) (Receipt, error) {
	w.calls++
	return Receipt{OperationID: mutation.OperationID, Provider: mutation.Provider, KeyID: mutation.KeyID, Outcome: "applied"}, nil
}

func TestSignedPlatformMutationAcceptsSecretOnlyInBody(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verifier, err := edgeauth.NewPlatformCredentialControlVerifier(map[string]ed25519.PublicKey{"operator": public}, time.Minute)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	writer := &writerFake{}
	server, err := New(verifier, writer, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := []byte(fmt.Sprintf(`{"schemaVersion":1,"operationId":"kpc_0123456789abcdef0123456789abcdef","provider":"cohere","keyId":"123e4567-e89b-42d3-a456-426614174000","secretBase64":"%s","class":"free","position":1,"operationActor":"operator:nate"}`, base64.StdEncoding.EncodeToString([]byte("secret"))))
	request := httptest.NewRequest(http.MethodPost, MutationPath, strings.NewReader(string(body)))
	millis := time.Now().UnixMilli()
	request.Header.Set(edgeauth.HeaderKeyID, "operator")
	request.Header.Set(edgeauth.HeaderTimestamp, fmt.Sprint(millis))
	request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, edgeauth.PlatformCredentialControlSigningInput("operator", millis, body))))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || writer.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, writer.calls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatal("response disclosed secret material")
	}
}

func TestCustomerControlSignatureCannotAuthorizePlatformMutation(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier, _ := edgeauth.NewPlatformCredentialControlVerifier(map[string]ed25519.PublicKey{"operator": public}, time.Minute)
	writer := &writerFake{}
	server, _ := New(verifier, writer, nil)
	body := []byte(`{}`)
	request := httptest.NewRequest(http.MethodPost, MutationPath, strings.NewReader(string(body)))
	millis := time.Now().UnixMilli()
	request.Header.Set(edgeauth.HeaderKeyID, "operator")
	request.Header.Set(edgeauth.HeaderTimestamp, fmt.Sprint(millis))
	request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, edgeauth.CredentialControlSigningInput("operator", millis, body))))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || writer.calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, writer.calls)
	}
}
