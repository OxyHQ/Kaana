package publicapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

type economicsSource struct {
	rows []credentialstore.CredentialEconomics
	err  error
}

func (e economicsSource) ReadCredentialEconomics(context.Context) ([]credentialstore.CredentialEconomics, error) {
	return e.rows, e.err
}
func TestNativeFundingUsesActualBindingAndLocalEvidenceOnly(t *testing.T) {
	pool, err := provider.NewKeyPool("stub", []provider.KeyDeclaration{{KeyID: "promo", Secret: "synthetic-promo-only", Funding: provider.FundingPromotional}, {KeyID: "prepaid", Secret: "synthetic-prepaid-only", Funding: provider.FundingPrepaid}, {KeyID: "free", Secret: "synthetic-free-only", Funding: provider.FundingFree}}, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &stubAdapter{pool: pool}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ReplaceGeneration([]provider.CredentialBinding{{DeploymentID: "exact_prepaid", Provider: "stub", KeyID: "prepaid"}}, adapter); err != nil {
		t.Fatal(err)
	}
	source := economicsSource{rows: []credentialstore.CredentialEconomics{{Provider: "stub", KeyID: "promo", Description: &credentialstore.CredentialEconomicsDescribed{CapacityCategory: "promotional"}, CapacityEvidence: json.RawMessage(`[]`)}, {Provider: "stub", KeyID: "prepaid", Description: &credentialstore.CredentialEconomicsDescribed{CapacityCategory: "prepaid"}, CapacityEvidence: json.RawMessage(`[]`)}}}
	f := publicapi.NativeFunding{Source: source, Registry: registry}
	at := time.Now()
	eligible, err := f.Eligibility(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible(provider.Route{DeploymentID: "default_pool", Provider: "stub"}) {
		t.Fatal("actual promotional pool excluded")
	}
	if eligible(provider.Route{DeploymentID: "exact_prepaid", Provider: "stub"}) {
		t.Fatal("promotional evidence escaped exact binding")
	}
	// Admission evidence does not narrow execution to the promotional key.
	for _, key := range []string{"free", "prepaid"} {
		bound, err := pool.Bind(key)
		if err != nil {
			t.Fatal(err)
		}
		limited, limitErr := bound.WithinFunding(provider.FundingTierCredit)
		if limitErr != nil || len(limited.UsableKeyIDs(at)) != 1 {
			t.Fatal("credit ceiling excluded legitimate free/prepaid execution")
		}
	}
	source.rows[0].CapacityEvidence = json.RawMessage(`[{"kind":"balance","balancePicos":"0"}]`)
	f.Source = source
	eligible, err = f.Eligibility(context.Background(), at)
	if err != nil || eligible(provider.Route{DeploymentID: "default_pool", Provider: "stub"}) {
		t.Fatal("zero promo capacity admitted")
	}
	f.Source = economicsSource{err: errors.New("database unavailable")}
	if _, err = f.Eligibility(context.Background(), at); err == nil {
		t.Fatal("read failure created funding evidence")
	}
}

func TestOrdinaryNativeFundingWithoutCeilingPreservesEveryExistingTier(t *testing.T) {
	for _, category := range []provider.Funding{provider.FundingFree, provider.FundingPrepaid, provider.FundingPaid, provider.FundingUnstated} {
		pool, err := provider.NewKeyPool("stub", []provider.KeyDeclaration{{KeyID: "fixture", Secret: "synthetic-only", Funding: category}}, provider.KeyPolicy{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := provider.NewRegistry(&stubAdapter{pool: pool})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := (publicapi.RegistryFunding{Registry: registry}).CheapestTier(provider.Route{DeploymentID: "fixture", Provider: "stub"}, provider.FundingTierNone); !ok {
			t.Fatalf("ordinary ceiling excluded %s", category)
		}
	}
}
