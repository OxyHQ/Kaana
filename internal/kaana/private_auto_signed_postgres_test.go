package kaana_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/httpapi"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/realtime"
	"github.com/OxyHQ/Kaana/internal/rotation"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real signed HTTP, owned PostgreSQL migrator/runtime roles and actual executor;
// only the provider exchange is synthetic. A declined parent never sends.
func TestPrivateAutoSignedHTTPPermanentSQLChild(t *testing.T) {
	databaseURL := os.Getenv("KAANA_PRIVATE_AUTO_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_PRIVATE_AUTO_TEST_URL owned PostgreSQL required")
	}
	raw, err := os.ReadFile("../contract/testdata/private-auto/golden-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var approval contract.PrivateAutoSourceApproval
	if err = json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	approval.DeploymentID = "synthetic-private-auto-deployment"
	approval.KeyID = "private-auto-key"
	approval.ModelReference = "typesafe/jev-1.13@2026-09-17"
	approval.UpstreamModelID = "typesafe/jev-1.13-20260917"
	approval.ProviderRateCardVersionID = "private-auto-card"
	approval.ProviderSourceVersion = "private-auto-observed"
	at := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	admin, err := credentialstore.OpenPostgres(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,key_class,enabled) VALUES('openrouter',$1,'\x01','arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000001',1,'paid',true)`, approval.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	var binding string
	err = pool.QueryRow(ctx, `SELECT kaana_bind_provider_deployment('kdb_000000000000000000000000000000aa',$1,'openrouter',$2,'synthetic-integration-test')`, approval.DeploymentID, approval.KeyID).Scan(&binding)
	if err != nil || binding != "applied" {
		t.Fatal(err, binding)
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("options", "-c role=kaana_runtime")
	u.RawQuery = query.Encode()
	runtime, err := credentialstore.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	deployment := inventory.Deployment{PrivateAutoSourceApproval: &approval, DeploymentID: approval.DeploymentID, Provider: approval.Provider, ModelReference: approval.ModelReference, UpstreamModelID: approval.UpstreamModelID, Regions: []contract.Region{}}
	encoded, err := json.Marshal(map[string]any{"snapshotId": "synthetic-private-auto", "issuedAt": contract.NewTimestamp(at), "deployments": []inventory.Deployment{deployment}})
	if err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(t.TempDir(), "inventory.json")
	if err = os.WriteFile(inventoryPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	store, err := inventory.NewStore(inventory.Config{Path: inventoryPath, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	keyPool, err := provider.NewKeyPool("openrouter", []provider.KeyDeclaration{{KeyID: approval.KeyID, Secret: "synthetic-provider-only"}}, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &wireProvider{pool: keyPool, claims: pool}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ReplaceGeneration([]provider.CredentialBinding{{DeploymentID: approval.DeploymentID, Provider: "openrouter", KeyID: approval.KeyID}}, adapter); err != nil {
		t.Fatal(err)
	}
	cardRaw, err := json.Marshal(map[string]any{"schemaVersion": 1, "rateCardVersionId": approval.ProviderRateCardVersionID, "source": "operator", "sourceVersion": approval.ProviderSourceVersion, "observedAt": at.Format(time.RFC3339), "effectiveAt": at.Format(time.RFC3339), "rateCards": []any{map[string]any{"deploymentId": approval.DeploymentID, "currency": "USD", "rates": []any{map[string]any{"unit": "input_tokens", "amountPerUnit": 42000}, map[string]any{"unit": "output_tokens", "amountPerUnit": 0}}}}})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Parse(cardRaw)
	if err != nil {
		t.Fatal(err)
	}
	rotations := rotation.NewRegistry(rotation.Policy{}, nil)
	claims := &observedWireClaims{repository: runtime}
	executor, err := kaana.NewExecutor(kaana.Config{Inventory: store, Providers: registry, Rotation: rotations, Costs: cards, ScopedAttemptClaims: claims})
	if err != nil {
		t.Fatal(err)
	}
	kaana.SetPrivateAutoApprovalForTest(executor, approval)

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"synthetic-oxy-wire": public}
	verifier, err := edgeauth.NewVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	validation, err := edgeauth.NewCredentialValidationVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	telemetry, err := edgeauth.NewProviderTelemetryVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := realtime.NewManager(realtime.Config{Opener: executor, Verifier: verifier, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(httpapi.Config{Executor: executor, Verifier: verifier, ValidationVerifier: validation, CredentialValidator: noCredentialValidation{}, TelemetryVerifier: telemetry, Telemetry: admin, Registry: registry, Inventory: store, Rotation: rotations, Realtime: sessions, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	golden, err := os.ReadFile("../contract/testdata/private-auto/golden-first.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire contract.PrivateAutoRequest
	if err = json.Unmarshal(golden, &wire); err != nil {
		t.Fatal(err)
	}
	send := func(parent string, mutate func(*contract.PrivateAutoRequest)) []byte {
		t.Helper()
		now := time.Now().UTC().Truncate(time.Millisecond)
		child := &wire.PrivateAutoExecution
		child.PrivateAutoAuthority = approval.PrivateAutoAuthority
		child.ParentMeteredUsageID = parent
		child.OperationID, _ = contract.PrivateAutoOperationID(parent)
		child.RequestID = contract.RequestID(child.OperationID)
		child.ApprovalSHA256, _ = approval.SHA256()
		child.SnapshotID = store.Current().SnapshotID()
		child.RuntimeExpiresAt = now.Add(time.Second).Format(time.RFC3339Nano)
		wire.Attribution.RequestID = child.RequestID
		id := contract.IdempotencyKey(child.OperationID)
		wire.IdempotencyKey = &id
		wire.Target.ModelReference = &child.ModelReference
		wire.Client.ReceivedAt = contract.NewTimestamp(now)
		wire.AuthorizedRoutes = []contract.AuthorizedRoute{{Provider: approval.Provider, DeploymentID: approval.DeploymentID, ModelReference: approval.ModelReference, Regions: []contract.Region{}, Substitution: contract.SubstitutionSameModel}}
		input, _ := json.Marshal(wire.Input)
		hash := sha256.Sum256(input)
		child.InputSHA256 = hex.EncodeToString(hash[:])
		if mutate != nil {
			mutate(&wire)
		}
		body, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var check contract.PrivateAutoRequest
		if e := json.Unmarshal(body, &check); e != nil && mutate == nil {
			t.Fatal("positive wire validation", e)
		}
		request, err := http.NewRequest(http.MethodPost, server.URL+"/internal/v1/decisions", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		timestamp := time.Now().UnixMilli()
		millis := strconv.FormatInt(timestamp, 10)
		request.Header.Set(edgeauth.HeaderKeyID, "synthetic-oxy-wire")
		request.Header.Set(edgeauth.HeaderTimestamp, millis)
		request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, edgeauth.SigningInput("synthetic-oxy-wire", timestamp, body))))
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	parent := "018f2118-95bc-7aca-914e-17632106cad8"
	result := send(parent, nil)
	if adapter.sends.Load() != 1 || !bytes.Contains(result, []byte(`"auto-power-level"`)) {
		t.Fatalf("positive child did not produce decision: sends=%d result=%s", adapter.sends.Load(), result)
	}
	operation, _ := contract.PrivateAutoOperationID(parent)
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM scoped_provider_attempt_claims WHERE operation_id=$1`, operation).Scan(&count); err != nil || count != 1 {
		t.Fatal("claim missing", count, err)
	}
	// Simulate a retired SQL lease; its permanent operation PK must still deny a
	// new input, new approval revision and freshly signed child for this parent.
	if _, err = pool.Exec(ctx, `UPDATE scoped_provider_attempt_claims SET expires_at=claimed_at+interval '1 millisecond' WHERE operation_id=$1`, operation); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	approval.ApprovalVersion++
	kaana.SetPrivateAutoApprovalForTest(executor, approval)

	updated, err := json.Marshal(map[string]any{"snapshotId": "synthetic-private-auto-revision2", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": []inventory.Deployment{deployment}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(inventoryPath, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if err = store.Reload(); err != nil {
		t.Fatal(err)
	}
	wire.Input.Decisions.State = "another synthetic task"
	_ = send(parent, nil)
	if adapter.sends.Load() != 1 || claims.calls.Load() != 2 {
		t.Fatalf("expired SQL lease reopened parent: sends=%d claims=%d", adapter.sends.Load(), claims.calls.Load())
	}
	for _, mutation := range []func(*contract.PrivateAutoRequest){
		func(r *contract.PrivateAutoRequest) { r.PrivateAutoExecution.Principal.AccountID = "foreign" },
		func(r *contract.PrivateAutoRequest) {
			r.PrivateAutoExecution.RuntimeExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
		},
		func(r *contract.PrivateAutoRequest) { r.PrivateAutoExecution.SnapshotID = "foreign" },
		func(r *contract.PrivateAutoRequest) {
			future := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
			r.Client.ReceivedAt = contract.NewTimestamp(future)
			r.PrivateAutoExecution.RuntimeExpiresAt = future.Add(time.Second).Format(time.RFC3339Nano)
		},
		func(r *contract.PrivateAutoRequest) { r.PrivateAutoExecution.ProviderRateCardVersionID = "foreign" },
	} {
		_ = send("018f2118-95bc-7aca-914e-17632106cad9", mutation)
		if adapter.sends.Load() != 1 || claims.calls.Load() != 2 {
			t.Fatal("invalid child reached SQL/provider")
		}
	}
	_ = send("018f2118-95bc-7aca-914e-17632106cadd", nil)
	if adapter.sends.Load() != 2 || claims.calls.Load() != 3 {
		t.Fatal("new legitimate parent refused", adapter.sends.Load(), claims.calls.Load())
	}
	t.Log("real signed HTTP and runtime SQL: two legitimate parents each one permanent child claim/send; changed approval and input replay after lease expiry and foreign/expired authority denied before provider")
}
