package publicapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// NativeCredentialEconomics reads local, label-free provider funding evidence.
// It is never an Oxy economic authorization request.
type NativeCredentialEconomics interface {
	ReadCredentialEconomics(context.Context) ([]credentialstore.CredentialEconomics, error)
}

type NativeFunding struct {
	Source   NativeCredentialEconomics
	Registry *provider.Registry
}

func (f NativeFunding) Eligibility(ctx context.Context, at time.Time) (func(provider.Route) bool, error) {
	if f.Source == nil || f.Registry == nil {
		return nil, errors.New("native funding requires local economics and registry")
	}
	rows, err := f.Source.ReadCredentialEconomics(ctx)
	if err != nil {
		return nil, err
	}
	evidence := make([]nativeadmission.CredentialFundingEvidence, 0, len(rows))
	for _, row := range rows {
		if row.Description == nil {
			continue
		}
		entry := nativeadmission.CredentialFundingEvidence{Provider: row.Provider, KeyID: row.KeyID, CapacityCategory: row.Description.CapacityCategory}
		var raw []struct {
			Kind           string      `json:"kind"`
			ObservedAt     time.Time   `json:"observedAt"`
			FreshUntil     *time.Time  `json:"freshUntil"`
			QuotaWindow    string      `json:"quotaWindow"`
			QuotaRemaining json.Number `json:"quotaRemaining"`
			BalancePicos   string      `json:"balancePicos"`
			ExpiresAt      *time.Time  `json:"expiresAt"`
		}
		if err := json.Unmarshal(row.CapacityEvidence, &raw); err != nil {
			return nil, errors.New("native funding capacity evidence is malformed")
		}
		for _, value := range raw {
			capacity := nativeadmission.FundingCapacity{Kind: value.Kind, ObservedAt: value.ObservedAt, QuotaWindow: value.QuotaWindow, QuotaRemaining: string(value.QuotaRemaining), BalancePicos: value.BalancePicos}
			if value.FreshUntil != nil {
				capacity.FreshUntil = *value.FreshUntil
			}
			if value.ExpiresAt != nil {
				capacity.ExpiresAt = *value.ExpiresAt
			}
			entry.Capacity = append(entry.Capacity, capacity)
		}
		evidence = append(evidence, entry)
	}
	return func(route provider.Route) bool {
		pool, err := f.Registry.ResolveCredential(route.DeploymentID, route.Provider)
		if err != nil || pool == nil {
			return false
		}
		keys := pool.UsableKeyIDs(at)
		return nativeadmission.PromotionalCreditAvailable(evidence, route.Provider, keys, at, at)
	}, nil
}
