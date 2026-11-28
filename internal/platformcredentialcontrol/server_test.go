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
	"github.com/jackc/pgx/v5/pgconn"
)

type writerFake struct {
	calls int
	err   error
}

func (w *writerFake) Import(_ context.Context, mutation Mutation, secret []byte) (Receipt, error) {
	w.calls++
	if w.err != nil {
		return Receipt{}, w.err
	}
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

func TestReadOnlyDatabaseAnswers503WithRetryAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		status     int
		retryAfter string
		body       string
	}{
		"readOnly": {fmt.Errorf("credential store: importing: %w", &pgconn.PgError{Code: "25006"}), http.StatusServiceUnavailable, "5", `{"code":"service_unavailable"}`},
		"other":    {fmt.Errorf("credential store: importing: %w", &pgconn.PgError{Code: "57P01"}), http.StatusInternalServerError, "", `{"code":"internal_error"}`},
	} {
		t.Run(name, func(t *testing.T) {
			public, private, _ := ed25519.GenerateKey(rand.Reader)
			verifier, _ := edgeauth.NewPlatformCredentialControlVerifier(map[string]ed25519.PublicKey{"operator": public}, time.Minute)
			writer := &writerFake{err: tc.err}
			server, _ := New(verifier, writer, nil)
			body := []byte(fmt.Sprintf(`{"schemaVersion":1,"operationId":"kpc_0123456789abcdef0123456789abcdef","provider":"cohere","keyId":"123e4567-e89b-42d3-a456-426614174000","secretBase64":"%s","class":"free","position":1,"operationActor":"operator:nate"}`, base64.StdEncoding.EncodeToString([]byte("secret"))))
			request := httptest.NewRequest(http.MethodPost, MutationPath, strings.NewReader(string(body)))
			millis := time.Now().UnixMilli()
			request.Header.Set(edgeauth.HeaderKeyID, "operator")
			request.Header.Set(edgeauth.HeaderTimestamp, fmt.Sprint(millis))
			request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, edgeauth.PlatformCredentialControlSigningInput("operator", millis, body))))
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.status || response.Header().Get("Retry-After") != tc.retryAfter || strings.TrimSpace(response.Body.String()) != tc.body || writer.calls != 1 {
				t.Fatalf("status=%d Retry-After=%q body=%s calls=%d", response.Code, response.Header().Get("Retry-After"), response.Body.String(), writer.calls)
			}
		})
	}
}
