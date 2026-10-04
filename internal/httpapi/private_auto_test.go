package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrivateAutoNegotiationIndependentAndSourceBound(t *testing.T) {
	raw, err := os.ReadFile("../contract/testdata/private-auto/golden-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var approval contract.PrivateAutoSourceApproval
	if err = json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	deployment := inventory.Deployment{PrivateAutoSourceApproval: &approval, DeploymentID: approval.DeploymentID, Provider: approval.Provider, ModelReference: approval.ModelReference, UpstreamModelID: approval.UpstreamModelID, Regions: []contract.Region{}}
	raw, err = json.Marshal(map[string]any{"snapshotId": "private-auto-http", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []inventory.Deployment{deployment}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	store, err := inventory.NewStore(inventory.Config{Path: path, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := edgeauth.NewVerifier(map[string]ed25519.PublicKey{"test": public}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{inventory: store, verifier: verifier, logger: logger, maxEnvelopeBytes: 1 << 20}
	request := func(body string, models bool, sign bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/internal/v1/deployments/query", strings.NewReader(body))
		if sign {
			at := time.Now().UnixMilli()
			req.Header.Set(edgeauth.HeaderKeyID, "test")
			req.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(at, 10))
			req.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, edgeauth.SigningInput("test", at, []byte(body)))))
		}
		response := httptest.NewRecorder()
		if models {
			server.handleScopedModels(response, req)
		} else {
			server.handleDeployments(response, req)
		}
		return response
	}
	negotiate := `{"privateAutoExecutionContractVersion":"3.7.0"}`
	if request(negotiate, false, true).Code != 400 || request(negotiate, true, true).Code != 400 {
		t.Fatal("nil production source negotiated private Auto")
	}
	server.privateAutoSource = func() *contract.PrivateAutoSourceApproval { return &approval }
	for _, models := range []bool{false, true} {
		positive := request(negotiate, models, true)
		if positive.Code != 200 || !bytes.Contains(positive.Body.Bytes(), []byte(`"privateAutoSourceApproval"`)) || !bytes.Contains(positive.Body.Bytes(), []byte(`"privateAutoExecutionContractVersion":"3.7.0"`)) {
			t.Fatal("signed source negotiation failed", positive.Code, positive.Body.String())
		}
		if file := os.Getenv("KAANA_PRIVATE_AUTO_NEGOTIATION_FIXTURE"); file != "" && !models {
			out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := out.Write(positive.Body.Bytes())
			closeErr := out.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatal(writeErr, closeErr)
			}
		}
		if request(negotiate, models, false).Code != 401 {
			t.Fatal("unsigned negotiation accepted")
		}
	}
	legacy := request(`{}`, false, true)
	if legacy.Code != 200 || bytes.Contains(legacy.Body.Bytes(), []byte("privateAuto")) {
		t.Fatal("ordinary metadata changed")
	}
	scoped := request(`{"scopedExecutionContractVersion":"3.6.0"}`, false, true)
	if scoped.Code != 200 || bytes.Contains(scoped.Body.Bytes(), []byte("privateAuto")) {
		t.Fatal("commissioning inherited private Auto")
	}
	for _, body := range []string{`{"privateAutoExecutionContractVersion":"3.6.0"}`, `{"privateAutoExecutionContractVersion":"3.7.0","scopedExecutionContractVersion":"3.6.0"}`, `{"privateAutoExecutionContractVersion":"3.7.0","privateAutoExecutionContractVersion":"3.7.0"}`, `{"privateAutoExecutionContractVersion":null}`} {
		if request(body, false, true).Code != 400 {
			t.Fatal("ambiguous extension accepted")
		}
	}
	foreign := approval
	foreign.ApprovalVersion++
	server.privateAutoSource = func() *contract.PrivateAutoSourceApproval { return &foreign }
	projection := request(negotiate, false, true)
	encoded, _ := io.ReadAll(projection.Body)
	if bytes.Contains(encoded, []byte(`"privateAutoSourceApproval"`)) {
		t.Fatal("foreign approval projection exposed")
	}
}
