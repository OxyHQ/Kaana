package credentialcontrol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
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
	operationID string
	rotateErr   error
	outcome     credentialstore.CustomerCredentialOutcome
	outcomeErr  error
}

func (w *fakeMutationWriter) Create(_ context.Context, operationID string, identity credentialstore.CustomerCredentialIdentity, secret []byte, actor string) (credentialstore.CustomerCredentialOutcome, error) {
	w.createCalls++
	w.operationID = operationID
	w.identity = identity
	w.secret = append([]byte(nil), secret...)
	w.actor = actor
	w.outcome = appliedOutcome(operationID, credentialstore.CustomerCredentialActionCreate, identity, "", 0, secret)
	return w.outcome, nil
}

func (w *fakeMutationWriter) Rotate(_ context.Context, operationID string, scope credentialstore.CustomerCredentialScope, secret []byte, actor string) (credentialstore.CustomerCredentialOutcome, error) {
	w.rotateCalls++
	w.operationID = operationID
	w.scope = scope
	w.secret = append([]byte(nil), secret...)
	w.actor = actor
	if w.rotateErr != nil {
		if errors.Is(w.rotateErr, credentialstore.ErrCustomerCredentialConflict) {
			return credentialstore.CustomerCredentialOutcome{
				Operation: operationForTest(operationID, credentialstore.CustomerCredentialActionRotate, scope, secret),
				Status:    credentialstore.CustomerCredentialOutcomeConflict,
			}, w.rotateErr
		}
		return credentialstore.CustomerCredentialOutcome{}, w.rotateErr
	}
	w.outcome = appliedOutcome(operationID, credentialstore.CustomerCredentialActionRotate, scope.CustomerCredentialIdentity, scope.CredentialHandle, scope.Revision, secret)
	return w.outcome, nil
}

func (w *fakeMutationWriter) Revoke(_ context.Context, operationID string, scope credentialstore.CustomerCredentialScope, actor string) (credentialstore.CustomerCredentialOutcome, error) {
	w.revokeCalls++
	w.operationID = operationID
	w.scope = scope
	w.actor = actor
	w.outcome = appliedOutcome(operationID, credentialstore.CustomerCredentialActionRevoke, scope.CustomerCredentialIdentity, scope.CredentialHandle, scope.Revision, nil)
	return w.outcome, nil
}

func (w *fakeMutationWriter) Outcome(_ context.Context, query credentialstore.CustomerCredentialOutcomeQuery) (credentialstore.CustomerCredentialOutcome, error) {
	if w.outcomeErr != nil {
		return credentialstore.CustomerCredentialOutcome{}, w.outcomeErr
	}
	if outcomeQueryForTest(w.outcome.Operation) != query {
		return credentialstore.CustomerCredentialOutcome{}, credentialstore.ErrCustomerCredentialOutcomeUnavailable
	}
	return w.outcome, nil
}

func outcomeQueryForTest(operation credentialstore.CustomerCredentialOperation) credentialstore.CustomerCredentialOutcomeQuery {
	return credentialstore.CustomerCredentialOutcomeQuery{
		OperationID: operation.OperationID, Action: operation.Action,
		CustomerCredentialIdentity: operation.CustomerCredentialIdentity,
		CredentialHandle:           operation.CredentialHandle,
		ExpectedRevision:           operation.ExpectedRevision,
	}
}

func appliedOutcome(operationID string, action credentialstore.CustomerCredentialAction, identity credentialstore.CustomerCredentialIdentity, handle string, expectedRevision int64, secret []byte) credentialstore.CustomerCredentialOutcome {
	operation := credentialstore.CustomerCredentialOperation{
		OperationID: operationID, Action: action, CustomerCredentialIdentity: identity,
		CredentialHandle: handle, ExpectedRevision: expectedRevision,
	}
	if secret != nil {
		digest := sha256.Sum256(secret)
		operation.SecretSHA256 = hex.EncodeToString(digest[:])
	}
	if action == credentialstore.CustomerCredentialActionCreate {
		handle = controlTestHandle
	}
	return credentialstore.CustomerCredentialOutcome{
		Operation: operation, Status: credentialstore.CustomerCredentialOutcomeApplied,
		CredentialHandle: handle, Revision: expectedRevision + 1,
	}
}

func operationForTest(operationID string, action credentialstore.CustomerCredentialAction, scope credentialstore.CustomerCredentialScope, secret []byte) credentialstore.CustomerCredentialOperation {
	operation := credentialstore.CustomerCredentialOperation{
		OperationID: operationID, Action: action,
		CustomerCredentialIdentity: scope.CustomerCredentialIdentity,
		CredentialHandle:           scope.CredentialHandle, ExpectedRevision: scope.Revision,
	}
	if secret != nil {
		digest := sha256.Sum256(secret)
		operation.SecretSHA256 = hex.EncodeToString(digest[:])
	}
	return operation
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

func (h controlHarness) requestAt(t *testing.T, path string, body string, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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

func (h controlHarness) request(t *testing.T, body string, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	return h.requestAt(t, MutationPath, body, signed)
}

func TestSignedCreateAcceptsSecretTransientlyAndReturnsOnlyOpaqueReference(t *testing.T) {
	harness := newControlHarness(t)
	plaintext := "customer-provider-secret"
	body := `{"schemaVersion":1,"action":"create","operationId":"operation_create_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:acc_customer_01","secretBase64":"` + base64.StdEncoding.EncodeToString([]byte(plaintext)) + `"}`
	response := harness.request(t, body, true)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if harness.writer.createCalls != 1 || harness.writer.operationID != "operation_create_01" || string(harness.writer.secret) != plaintext {
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

func TestSecretValueRequiresCanonicalPaddedBase64(t *testing.T) {
	var canonical contract.KaanaCredentialSecret
	if err := json.Unmarshal([]byte(`"YQ=="`), &canonical); err != nil || string(canonical) != "a" {
		t.Fatalf("canonical base64 decoded to %q with %v", canonical, err)
	}
	canonical.Clear()

	for name, encoded := range map[string]string{
		"non-zero trailing bits": `"YR=="`,
		"missing padding":        `"YQ"`,
		"embedded newline":       `"YQ\\n=="`,
		"extra padding":          `"YQ==="`,
		"decoded whitespace":     `"IA=="`,
	} {
		t.Run(name, func(t *testing.T) {
			var secret contract.KaanaCredentialSecret
			if err := json.Unmarshal([]byte(encoded), &secret); err == nil {
				secret.Clear()
				t.Fatalf("non-canonical base64 %s was accepted", encoded)
			}
		})
	}
}

func TestUnsignedMutationIsRejectedBeforeTheWriter(t *testing.T) {
	harness := newControlHarness(t)
	body := `{"schemaVersion":1,"action":"revoke","operationId":"operation_revoke_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":1}`
	response := harness.request(t, body, false)
	if response.Code != http.StatusUnauthorized || harness.writer.revokeCalls != 0 {
		t.Fatalf("status/calls = %d/%d", response.Code, harness.writer.revokeCalls)
	}
}

func TestMutationContractRejectsUnknownDuplicateAndCrossActionFields(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":    `{"schemaVersion":1,"action":"revoke","operationId":"operation_revoke_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":1,"providerName":"Anthropic"}`,
		"duplicate handle": `{"schemaVersion":1,"action":"revoke","operationId":"operation_revoke_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","credentialHandle":"` + controlTestHandle + `","expectedRevision":1}`,
		"secret on revoke": `{"schemaVersion":1,"action":"revoke","operationId":"operation_revoke_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":1,"secretBase64":"c2VjcmV0"}`,
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

func TestRotationConflictReturnsTheExactOperationOutcome(t *testing.T) {
	harness := newControlHarness(t)
	body := `{"schemaVersion":1,"action":"rotate","operationId":"operation_rotate_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":4,"secretBase64":"` + base64.StdEncoding.EncodeToString([]byte("rotated-secret")) + `"}`
	first := harness.request(t, body, true)
	if first.Code != http.StatusOK || harness.writer.scope.Revision != 4 {
		t.Fatalf("first status/scope = %d/%#v", first.Code, harness.writer.scope)
	}
	harness.writer.rotateErr = credentialstore.ErrCustomerCredentialConflict
	second := harness.request(t, body, true)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), `"status":"conflict"`) || !strings.Contains(second.Body.String(), `"operationId":"operation_rotate_01"`) {
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
	body := `{"schemaVersion":1,"action":"rotate","operationId":"operation_rotate_01","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":4,"secretBase64":"` + base64.StdEncoding.EncodeToString([]byte("customer-provider-secret")) + `"}`
	response := harness.request(t, body, true)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String()+harness.logs.String(), "customer-provider-secret") || strings.Contains(response.Body.String()+harness.logs.String(), "kms refused") {
		t.Fatal("an internal credential error escaped")
	}
}

func TestSignedOutcomeReconcilesLostResponseOnlyForExactIdentity(t *testing.T) {
	harness := newControlHarness(t)
	mutation := `{"schemaVersion":1,"action":"rotate","operationId":"operation_rotate_lost","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","credentialHandle":"` + controlTestHandle + `","expectedRevision":4,"secretBase64":"` + base64.StdEncoding.EncodeToString([]byte("rotated-secret")) + `"}`
	if response := harness.request(t, mutation, true); response.Code != http.StatusOK {
		t.Fatalf("mutation status/body = %d/%s", response.Code, response.Body.String())
	}
	query := `{"schemaVersion":1,"action":"rotate","operationId":"operation_rotate_lost","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","credentialHandle":"` + controlTestHandle + `","expectedRevision":4}`
	response := harness.requestAt(t, OutcomePath, query, true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"applied"`) || !strings.Contains(response.Body.String(), `"revision":5`) {
		t.Fatalf("outcome status/body = %d/%s", response.Code, response.Body.String())
	}
	wrongIdentity := strings.Replace(query, "acc_customer_01", "acc_other", 1)
	response = harness.requestAt(t, OutcomePath, wrongIdentity, true)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), controlTestHandle) {
		t.Fatalf("wrong-identity status/body = %d/%s", response.Code, response.Body.String())
	}
	legacyFingerprint := strings.Replace(query, `"credentialHandle"`, `"secretSha256":"`+strings.Repeat("0", 64)+`","credentialHandle"`, 1)
	response = harness.requestAt(t, OutcomePath, legacyFingerprint, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("legacy-fingerprint status/body = %d/%s", response.Code, response.Body.String())
	}
}

func TestCredentialControlRejectsOversizedSignedBody(t *testing.T) {
	harness := newControlHarness(t)
	body := `{"schemaVersion":1,"action":"create","operationId":"operation_large","provider":"anthropic","ownerAccountId":"acc_customer_01","connectionId":"conn_customer_01","environment":"production","operationActor":"user:owner","secretBase64":"` + strings.Repeat("a", int(maxMutationBytes)) + `"}`
	response := harness.request(t, body, true)
	if response.Code != http.StatusBadRequest || harness.writer.createCalls != 0 {
		t.Fatalf("oversized status/calls = %d/%d", response.Code, harness.writer.createCalls)
	}
}
