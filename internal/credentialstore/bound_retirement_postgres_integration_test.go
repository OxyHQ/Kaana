package credentialstore

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// boundRetirementAdapter is the least an adapter needs to hold a platform pool
// in a registry generation. The walk under test is provider.Walk itself.
type boundRetirementAdapter struct {
	slug contract.ProviderSlug
	pool *provider.KeyPool
}

func (a boundRetirementAdapter) Provider() contract.ProviderSlug { return a.slug }
func (boundRetirementAdapter) Translate(*contract.Request, provider.Route) (*provider.Call, error) {
	return nil, errors.New("not used")
}
func (boundRetirementAdapter) Stream(context.Context, *provider.Call, provider.Emitter, *provider.KeyPool) (provider.Outcome, error) {
	return provider.Outcome{}, errors.New("not used")
}
func (a boundRetirementAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: a.slug}
}
func (a boundRetirementAdapter) PlatformCredentials() *provider.KeyPool { return a.pool }

// boundRetirementSender answers every call with one provider refusal, already
// classified the way the adapters classify it.
type boundRetirementSender struct {
	status int
	code   contract.ErrorCode
	cat    contract.UpstreamErrorCategory
}

func (s boundRetirementSender) Send(context.Context, *provider.Call, provider.Key) (*http.Response, error) {
	return &http.Response{StatusCode: s.status, Header: make(http.Header), Body: http.NoBody}, nil
}
func (s boundRetirementSender) Refuse(response *http.Response, _ provider.Key) error {
	_ = response.Body.Close()
	return provider.ErrUpstream{Code: s.code, Category: s.cat, Detail: "refused"}
}
func (boundRetirementSender) TransportFailure(_ context.Context, err error) error { return err }

// TestABoundRefusalRecordsThePoliciesRetirementInPostgres drives the schema
// 0013 execution path end to end against the real runtime functions: a pool
// loaded with its provider_key_policies row, a deployment resolved to an
// exact binding or to its provider's only key, and a provider refusal. The
// database refuses a retirement that ends when it begins, which is exactly
// what a bound view without its pool's policy recorded — and the refusal came
// back as an unclassified provider_error instead of the provider's verdict.
func TestABoundRefusalRecordsThePoliciesRetirementInPostgres(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET SESSION AUTHORIZATION kaana_runtime`)
		return err
	}
	runtimePool, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtimePool.Close)
	runtime := &Postgres{pool: runtimePool}

	const retirement = 20 * time.Minute
	cases := []struct {
		name     string
		slug     contract.ProviderSlug
		keyID    string
		binding  bool
		sender   boundRetirementSender
		outcome  string
		evidence string
	}{
		{
			name: "exact binding billing refusal", slug: "bound-retirement-exact", keyID: "123e4567-e89b-42d3-a456-426614175001", binding: true,
			sender:  boundRetirementSender{http.StatusPaymentRequired, contract.CodeProviderBillingRefused, contract.UpstreamQuota},
			outcome: "exhausted", evidence: "provider_error",
		},
		{
			name: "provider default credential rejection", slug: "bound-retirement-default", keyID: "123e4567-e89b-42d3-a456-426614175002",
			sender:  boundRetirementSender{http.StatusUnauthorized, contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication},
			outcome: "rejected", evidence: "provider_error",
		},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,enabled) VALUES($1,$2,'x','arn:aws:kms:eu-west-1:123456789012:key/00000000-0000-0000-0000-000000000001',$3,true) ON CONFLICT(provider_slug,key_id) DO UPDATE SET enabled=true`, tc.slug, tc.keyID, 910000+index); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				c, cc := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cc()
				_, _ = pool.Exec(c, `DELETE FROM provider_credential_attempt_events WHERE provider_slug=$1`, tc.slug)
				_, _ = pool.Exec(c, `DELETE FROM provider_credential_runtime_state WHERE provider_slug=$1`, tc.slug)
				_, _ = pool.Exec(c, `DELETE FROM provider_key_policy_audit WHERE provider_slug=$1`, tc.slug)
				_, _ = pool.Exec(c, `DELETE FROM provider_key_policies WHERE provider_slug=$1`, tc.slug)
				_, _ = pool.Exec(c, `DELETE FROM provider_credentials WHERE provider_slug=$1`, tc.slug)
			})
			if err := repository.PutKeyPolicy(ctx, tc.slug, retirement, false, "operator:integration"); err != nil {
				t.Fatal(err)
			}
			policies, err := runtime.LoadKeyPolicies(ctx, []contract.ProviderSlug{tc.slug})
			if err != nil {
				t.Fatal(err)
			}
			keys, err := provider.NewKeyPool(tc.slug, []provider.KeyDeclaration{{
				KeyID: tc.keyID, Secret: "kaana-integration-fake-credential", Runtime: runtime,
			}}, policies[tc.slug], nil)
			if err != nil {
				t.Fatal(err)
			}
			adapter := boundRetirementAdapter{slug: tc.slug, pool: keys}
			registry, err := provider.NewRegistry(adapter)
			if err != nil {
				t.Fatal(err)
			}
			deployment := contract.DeploymentID("dep_" + string(tc.slug))
			var bindings []provider.CredentialBinding
			if tc.binding {
				bindings = append(bindings, provider.CredentialBinding{DeploymentID: deployment, Provider: tc.slug, KeyID: tc.keyID})
			}
			if err := registry.ReplaceGeneration(bindings, adapter); err != nil {
				t.Fatal(err)
			}
			_, view, err := registry.ResolveExecution(deployment, tc.slug, true)
			if err != nil {
				t.Fatal(err)
			}

			call := &provider.Call{RequestID: contract.RequestID("req_" + string(tc.slug)), Route: provider.Route{Provider: tc.slug, DeploymentID: deployment}}
			response, _, err := provider.Walk(ctx, view, call, tc.sender)
			if response != nil {
				_ = response.Body.Close()
				t.Fatal("a refused walk returned a response")
			}
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || upstream.Code != tc.sender.code {
				t.Fatalf("walk = %v; want %s", err, tc.sender.code)
			}

			var (
				state, evidence          string
				observedAt, retiredUntil time.Time
			)
			if err := pool.QueryRow(ctx, `SELECT state, evidence_source, observed_at, retired_until FROM provider_credential_runtime_state WHERE provider_slug=$1 AND key_id=$2`,
				tc.slug, tc.keyID).Scan(&state, &evidence, &observedAt, &retiredUntil); err != nil {
				t.Fatalf("no retirement was recorded: %v", err)
			}
			if state != tc.outcome || evidence != tc.evidence {
				t.Fatalf("recorded %s/%s, want %s/%s", state, evidence, tc.outcome, tc.evidence)
			}
			if got := retiredUntil.Sub(observedAt); got != retirement {
				t.Fatalf("recorded a retirement of %s, want the policy's %s", got, retirement)
			}
		})
	}
}
