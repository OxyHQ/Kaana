package kaana_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
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

// Only the provider is synthetic. Transport authentication, contract parsing,
// inventory/card/key matching, executor admission and durable SQL claims are real.
type wireProvider struct {
	pool   *provider.KeyPool
	sends  atomic.Int32
	claims *pgxpool.Pool
}

type observedWireClaims struct {
	repository *credentialstore.Postgres
	calls      atomic.Int32
}

func (c *observedWireClaims) ClaimScopedAttempt(ctx context.Context, claim credentialstore.ScopedAttemptClaim) (bool, error) {
	c.calls.Add(1)
	return c.repository.ClaimScopedAttempt(ctx, claim)
}

func (*wireProvider) Provider() contract.ProviderSlug { return "openrouter" }
func (*wireProvider) APIFormats() []contract.APIFormat {
	return []contract.APIFormat{contract.APIFormatDecisions}
}
func (p *wireProvider) PlatformCredentials() *provider.KeyPool { return p.pool }
func (*wireProvider) Health(context.Context) provider.Health   { return provider.Health{} }
func (*wireProvider) Translate(r *contract.Request, route provider.Route) (*provider.Call, error) {
	return &provider.Call{PrivateAutoExecution: r.PrivateAutoExecution, Route: route, Decisions: r.Input.Decisions}, nil
}
func (p *wireProvider) Stream(ctx context.Context, call *provider.Call, _ provider.Emitter, pool *provider.KeyPool) (provider.Outcome, error) {
	outcome, key, err := provider.WalkAttempts(ctx, pool, call, func(ctx context.Context, _ provider.Key) (provider.Outcome, provider.CredentialedAttempt) {
		var count int
		operation := ""
		if call.Route.ScopedExecution != nil {
			operation = call.Route.ScopedExecution.PermitID
		} else if call.PrivateAutoExecution != nil {
			operation = call.PrivateAutoExecution.OperationID
		}
		if err := p.claims.QueryRow(ctx, `SELECT count(*) FROM scoped_provider_attempt_claims WHERE operation_id=$1`, operation).Scan(&count); err != nil || count != 1 {
			return provider.Outcome{}, provider.CredentialedAttempt{Transport: true, Failure: errors.New("provider reached without durable SQL claim")}
		}
		p.sends.Add(1)
		answers := make([]contract.DecisionAnswer, 0, len(call.Decisions.Questions))
		for _, q := range call.Decisions.Questions {
			answer := contract.DecisionAnswer{ID: q.ID, Kind: q.Kind}
			switch q.Kind {
			case "choice":
				label := q.Options[0]
				confidence := 1.0
				answer.Reply = &contract.DecisionReply{Label: &label}
				answer.Confidence = &confidence
				answer.Probabilities = make([]float64, len(q.Options))
				answer.Probabilities[0] = 1
			case "score":
				mean := float64(len(q.Levels)-1) / 2
				confidence := 1.0
				answer.Reply = &contract.DecisionReply{Score: &mean}
				answer.Confidence = &confidence
				answer.Mean = &mean
				answer.Distribution = make([]float64, len(q.Levels))
				answer.Distribution[len(q.Levels)/2] = 1
			default:
				probability := .75
				answer.Probability = &probability
			}
			answers = append(answers, answer)
		}
		return provider.Outcome{Decisions: answers, Units: []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 10}, {Unit: contract.UnitOutputTokens, Quantity: 0}}, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, provider.CredentialedAttempt{Accepted: true}
	})
	outcome.KeyID = key.ID
	return outcome, err
}

// Not used by this decisions-only test; refuse rather than fabricate validation.
type noCredentialValidation struct{}

func (noCredentialValidation) Validate(context.Context, contract.KaanaCredentialValidationTask) (contract.KaanaCredentialValidationOutcome, error) {
	return contract.KaanaCredentialValidationOutcome{}, errors.New("not exercised by decisions integration")
}

func TestMentionOxySignedWireFixture(t *testing.T) {
	path := os.Getenv("MENTION_WIRE_FIXTURE")
	if path == "" {
		t.Skip("run through the owned Oxy/Mention integration operator")
	}
	var fixture struct {
		Audience    contract.ScopedExecutionAudience `json:"audience"`
		PublicKey   string                           `json:"publicKey"`
		DatabaseURL string                           `json:"databaseUrl"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if err = fixture.Audience.Validate(); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	admin, err := credentialstore.OpenPostgres(ctx, fixture.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, fixture.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,key_class,enabled) VALUES('openrouter',$1,'\x01','arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000001',1,'paid',true)`, fixture.Audience.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	var binding string
	err = pool.QueryRow(ctx, `SELECT kaana_bind_provider_deployment('kdb_000000000000000000000000000000ef',$1,'openrouter',$2,'synthetic-integration-test')`, fixture.Audience.DeploymentID, fixture.Audience.KeyID).Scan(&binding)
	if err != nil || binding != "applied" {
		t.Fatal(err, binding)
	}
	u, err := url.Parse(fixture.DatabaseURL)
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
	deployment := inventory.Deployment{ScopedExecution: &fixture.Audience, DeploymentID: fixture.Audience.DeploymentID, Provider: fixture.Audience.Provider, ModelReference: fixture.Audience.ModelReference, UpstreamModelID: fixture.Audience.UpstreamModelID, Regions: []contract.Region{}}
	encoded, err := json.Marshal(map[string]any{"snapshotId": "synthetic-mention-wire", "issuedAt": contract.NewTimestamp(at), "deployments": []inventory.Deployment{deployment}})
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
	keyPool, err := provider.NewKeyPool("openrouter", []provider.KeyDeclaration{{KeyID: fixture.Audience.KeyID, Secret: "synthetic-provider-only"}}, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &wireProvider{pool: keyPool, claims: pool}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ReplaceGeneration([]provider.CredentialBinding{{DeploymentID: fixture.Audience.DeploymentID, Provider: "openrouter", KeyID: fixture.Audience.KeyID}}, adapter); err != nil {
		t.Fatal(err)
	}
	cardRaw, err := json.Marshal(map[string]any{"schemaVersion": 1, "rateCardVersionId": fixture.Audience.ProviderRateCardVersionID, "source": "operator", "sourceVersion": fixture.Audience.ProviderSourceVersion, "observedAt": at.Format(time.RFC3339), "effectiveAt": at.Format(time.RFC3339), "rateCards": []any{map[string]any{"deploymentId": fixture.Audience.DeploymentID, "currency": "USD", "rates": []any{map[string]any{"unit": "input_tokens", "amountPerUnit": 42000}, map[string]any{"unit": "output_tokens", "amountPerUnit": 0}}}}})
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
	kaana.SetMentionAudienceForTest(executor, fixture.Audience)
	public, err := base64.StdEncoding.DecodeString(fixture.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"synthetic-oxy-wire": ed25519.PublicKey(public)}
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
	ready, _ := json.Marshal(map[string]any{"url": server.URL})
	if err = os.WriteFile(path+".ready", ready, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = os.Stat(path + ".stop"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Oxy fixture did not finish before deadline")
		case <-time.After(20 * time.Millisecond):
		}
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM scoped_provider_attempt_claims WHERE operation_id=$1`, fixture.Audience.PermitID).Scan(&count); err != nil || count != 1 || adapter.sends.Load() != 1 || claims.calls.Load() != 2 {
		t.Fatalf("expected one durable claim/send, two real SQL calls: count=%d sends=%d claims=%d err=%v", count, adapter.sends.Load(), claims.calls.Load(), err)
	}
	var widened bool
	if err = pool.QueryRow(ctx, `SELECT has_table_privilege('kaana_runtime','scoped_provider_attempt_claims','INSERT')`).Scan(&widened); err != nil || widened {
		t.Fatal("runtime claim privilege changed", err)
	}
	t.Log("real signed HTTP executor: one provider send, one permanent SQL claim, replay and foreign principal denied")
}
