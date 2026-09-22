package credentialstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProviderKeyPolicyPostgresLifecycle(t *testing.T) {
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
	const providerSlug = contract.ProviderSlug("key-policy-integration")
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = pool.Exec(c, `DELETE FROM provider_key_policy_audit WHERE provider_slug=$1`, providerSlug)
		_, _ = pool.Exec(c, `DELETE FROM provider_key_policies WHERE provider_slug=$1`, providerSlug)
	})

	// Absent: LoadKeyPolicies returns no entry, not an error or a zero row.
	policies, err := repository.LoadKeyPolicies(ctx, []contract.ProviderSlug{providerSlug})
	if err != nil {
		t.Fatalf("loading before any row exists: %v", err)
	}
	if _, present := policies[providerSlug]; present {
		t.Fatal("an absent policy row was returned as present")
	}

	if err := repository.PutKeyPolicy(ctx, providerSlug, 45*time.Minute, true, "operator:integration"); err != nil {
		t.Fatalf("put: %v", err)
	}
	policies, err = repository.LoadKeyPolicies(ctx, []contract.ProviderSlug{providerSlug})
	if err != nil {
		t.Fatalf("loading after put: %v", err)
	}
	policy, present := policies[providerSlug]
	if !present {
		t.Fatal("the put policy is absent from the load")
	}
	if policy.Retirement != 45*time.Minute || !policy.OnSeparateAccounts {
		t.Fatalf("loaded policy is %+v, expected 45m/true", policy)
	}

	// Upsert replaces, it does not merge.
	if err := repository.PutKeyPolicy(ctx, providerSlug, 5*time.Minute, false, "operator:integration"); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	listed, err := repository.ListKeyPolicies(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, row := range listed {
		if row.Provider != providerSlug {
			continue
		}
		found = true
		if row.KeyRetirementSeconds != 300 || row.KeysOnSeparateAccounts {
			t.Fatalf("listed row is %+v, expected 300s/false after the second put", row)
		}
	}
	if !found {
		t.Fatal("the second put is absent from the listing")
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_key_policy_audit WHERE provider_slug=$1`, providerSlug).Scan(&auditCount); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if auditCount != 2 {
		t.Fatalf("two puts left %d audit rows, expected 2", auditCount)
	}
}
