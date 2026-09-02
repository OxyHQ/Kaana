package credentialcontrol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

const (
	controlTestKeyID  = "edge-control-test"
	controlTestHandle = "kcred_abcdefghijklmnopqrstuvwxyz"
)

type fakeMutationWriter struct {
	createCalls int
	rotateCalls int
	revokeCalls int
	secret      []byte
	identity    credentialstore.CustomerCredentialIdentity
	scope       credentialstore.CustomerCredentialScope
	actor       string
	rotateErr   error
}

func (w *fakeMutationWriter) Create(_ context.Context, identity credentialstore.CustomerCredentialIdentity, secret []byte, actor string) (credentialstore.CustomerCredentialReference, error) {
	w.createCalls++
	w.identity = identity
	w.secret = append([]byte(nil), secret...)
	w.actor = actor
	return credentialstore.CustomerCredentialReference{CredentialHandle: controlTestHandle, Revision: 1}, nil
}

func (w *fakeMutationWriter) Rotate(_ context.Context, scope credentialstore.CustomerCredentialScope, secret []byte, actor string) (credentialstore.CustomerCredentialReference, error) {
	w.rotateCalls++
	w.scope = scope
	w.secret = append([]byte(nil), secret...)
	w.actor = actor
	if w.rotateErr != nil {
		return credentialstore.CustomerCredentialReference{}, w.rotateErr
	}
	return credentialstore.CustomerCredentialReference{CredentialHandle: scope.CredentialHandle, Revision: scope.Revision + 1}, nil
}

func (w *fakeMutationWriter) Revoke(_ context.Context, scope credentialstore.CustomerCredentialScope, actor string) (credentialstore.CustomerCredentialReference, error) {
	w.revokeCalls++
	w.scope = scope
	w.actor = actor
	return credentialstore.CustomerCredentialReference{CredentialHandle: scope.CredentialHandle, Revision: scope.Revision + 1}, nil
}

type controlHarness struct {
	handler http.Handler
	writer  *fakeMutationWriter
	private ed25519.PrivateKey
	logs    *bytes.Buffer
}

func newControlHarness(t *testing.T) controlHarness {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verifier, err := edgeauth.NewCredentialControlVerifier(map[string]ed25519.PublicKey{controlTestKeyID: public}, time.Minute)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	writer := &fakeMutationWriter{}
	logs := &bytes.Buffer{}
	server, err := New(verifier, writer, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return controlHarness{handler: server.Handler(), writer: writer, private: private, logs: logs}
}

func (h controlHarness) request(t *testing.T, body string, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, MutationPath, strings.NewReader(body))
	if signed {
		milliseconds := time.Now().UnixMilli()
		signature := ed25519.Sign(h.private, edgeauth.CredentialControlSigningInput(controlTestKeyID, milliseconds, []byte(body)))
		request.Header.Set(edgeauth.HeaderKeyID, controlTestKeyID)
		request.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
		request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(signature))
	}
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func TestSignedCreateAcceptsSecretTransientlyAndReturnsOnlyOpaqueReference(t *testing.T) {
	harness := newControlHarness(t)
	plaintext := "customer-provider-secret"
	body := `{"schemaVersion":1,"action":"create","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:acc_customer_01","secretBase64":"` + base64.StdEncoding.EncodeToString([]byte(plaintext)) + `"}`
	response := harness.request(t, body, true)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if harness.writer.createCalls != 1 || string(harness.writer.secret) != plaintext {
		t.Fatalf("writer calls/secret = %d/%q", harness.writer.createCalls, harness.writer.secret)
	}
	if harness.writer.identity.OwnerAccountID != "acc_customer_01" || harness.writer.identity.ConnectionID != "conn_customer_01" {
		t.Fatalf("writer identity = %#v", harness.writer.identity)
	}
	rendered := response.Body.String() + harness.logs.String()
	if strings.Contains(rendered, plaintext) || strings.Contains(rendered, base64.StdEncoding.EncodeToString([]byte(plaintext))) {
		t.Fatal("the credential or its transport encoding reached a response or log")
	}
	if !strings.Contains(response.Body.String(), controlTestHandle) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %s, cache = %q", response.Body.String(), response.Header().Get("Cache-Control"))
	}
}

func TestUnsignedMutationIsRejectedBeforeTheWriter(t *testing.T) {
	harness := newControlHarness(t)
	body := `{"schemaVersion":1,"action":"revoke","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":1}`
	response := harness.request(t, body, false)
	if response.Code != http.StatusUnauthorized || harness.writer.revokeCalls != 0 {
		t.Fatalf("status/calls = %d/%d", response.Code, harness.writer.revokeCalls)
	}
}

func TestMutationContractRejectsUnknownDuplicateAndCrossActionFields(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":    `{"schemaVersion":1,"action":"revoke","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":1,"providerName":"Anthropic"}`,
		"duplicate handle": `{"schemaVersion":1,"action":"revoke","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","credentialHandle":"` + controlTestHandle + `","expectedRevision":1}`,
		"secret on revoke": `{"schemaVersion":1,"action":"revoke","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":1,"secretBase64":"c2VjcmV0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			harness := newControlHarness(t)
			response := harness.request(t, body, true)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if harness.writer.createCalls+harness.writer.rotateCalls+harness.writer.revokeCalls != 0 {
				t.Fatal("invalid mutation reached the writer")
			}
		})
	}
}

func TestRotationReplayConflictsInsteadOfRotatingAgain(t *testing.T) {
	harness := newControlHarness(t)
	body := `{"schemaVersion":1,"action":"rotate","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":4,"secretBase64":"` + base64.StdEncoding.EncodeToString([]byte("rotated-secret")) + `"}`
	first := harness.request(t, body, true)
	if first.Code != http.StatusOK || harness.writer.scope.Revision != 4 {
		t.Fatalf("first status/scope = %d/%#v", first.Code, harness.writer.scope)
	}
	harness.writer.rotateErr = credentialstore.ErrCustomerCredentialConflict
	second := harness.request(t, body, true)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "credential_conflict") {
		t.Fatalf("replay status/body = %d/%s", second.Code, second.Body.String())
	}
}

func TestNoCredentialResolutionHTTPRouteExists(t *testing.T) {
	harness := newControlHarness(t)
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/customer-provider-credentials/"+controlTestHandle, nil)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("credential read route status = %d", response.Code)
	}
}

func TestInternalFailuresExposeNoErrorDetail(t *testing.T) {
	harness := newControlHarness(t)
	harness.writer.rotateErr = errors.New("kms refused customer-provider-secret")
	body := `{"schemaVersion":1,"action":"rotate","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":4,"secretBase64":"` + base64.StdEncoding.EncodeToString([]byte("customer-provider-secret")) + `"}`
	response := harness.request(t, body, true)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String()+harness.logs.String(), "customer-provider-secret") || strings.Contains(response.Body.String()+harness.logs.String(), "kms refused") {
		t.Fatal("an internal credential error escaped")
	}
}
