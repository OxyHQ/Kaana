package providertelemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providertelemetry"
)

type collectorFunc func(context.Context, providertelemetry.Credential, time.Time) ([]providertelemetry.Observation, error)

func (f collectorFunc) Collect(ctx context.Context, credential providertelemetry.Credential, now time.Time) ([]providertelemetry.Observation, error) {
	return f(ctx, credential, now)
}

func TestProviderFailureIsUnknownRatherThanZero(t *testing.T) {
	runner, err := providertelemetry.NewRunner(map[contract.ProviderSlug]providertelemetry.Collector{
		"cohere": collectorFunc(func(context.Context, providertelemetry.Credential, time.Time) ([]providertelemetry.Observation, error) {
			return nil, errors.New("upstream timeout containing details that must not escape")
		}),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	snapshot := runner.Collect(context.Background(), providertelemetry.Credential{Provider: "cohere", KeyID: "key-2", Secret: []byte("secret-value")}, now)
	if len(snapshot.Observations) != 1 {
		t.Fatalf("observations = %#v", snapshot.Observations)
	}
	observation := snapshot.Observations[0]
	if observation.Certainty != providertelemetry.CertaintyUnknown || observation.Amount != "" || observation.UnknownReason != "provider_unavailable" {
		t.Fatalf("failed collection became %#v", observation)
	}
}

func TestVersionedOfficialPriceAndExactBalanceRemainOperatorSafe(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	runner, err := providertelemetry.NewRunner(map[contract.ProviderSlug]providertelemetry.Collector{
		"mistral": collectorFunc(func(_ context.Context, credential providertelemetry.Credential, observed time.Time) ([]providertelemetry.Observation, error) {
			return []providertelemetry.Observation{
				{Provider: credential.Provider, KeyID: credential.KeyID, Kind: providertelemetry.KindBalance, UsageUnit: "currency:USD", Currency: "USD", Amount: "500.00", Provenance: providertelemetry.ProvenanceProviderAPI, Certainty: providertelemetry.CertaintyExact, ObservedAt: observed, FreshUntil: observed.Add(5 * time.Minute)},
				{Provider: credential.Provider, Kind: providertelemetry.KindPrice, DeploymentID: "mistral-large", ModelReference: "mistralai/mistral-large@2026-09-01", UsageUnit: "input_tokens", Currency: "USD", Amount: "0.000002", Provenance: providertelemetry.ProvenanceProviderAPI, Certainty: providertelemetry.CertaintyExact, SourceVersion: "billing-api-etag-42", ObservedAt: observed, FreshUntil: observed.Add(5 * time.Minute)},
			}, nil
		}),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runner.Collect(context.Background(), providertelemetry.Credential{Provider: "mistral", KeyID: "key-a", Secret: []byte("never-project-me")}, now)
	if len(snapshot.Observations) != 2 {
		t.Fatalf("observations = %#v", snapshot.Observations)
	}
	for _, observation := range snapshot.Observations {
		if observation.Certainty == providertelemetry.CertaintyUnknown {
			t.Fatalf("valid observation rejected: %#v", observation)
		}
	}
	projection, err := providertelemetry.MarshalControlledProjection(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projection), "never-project-me") || !strings.Contains(string(projection), `"keyId":"key-a"`) {
		t.Fatalf("controlled projection leaks a secret or loses opaque identity: %s", projection)
	}
}

func TestControlledProjectionRejectsMutationAndMissingIdentity(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	runner, err := providertelemetry.NewRunner(map[contract.ProviderSlug]providertelemetry.Collector{
		"cohere": collectorFunc(func(_ context.Context, credential providertelemetry.Credential, observed time.Time) ([]providertelemetry.Observation, error) {
			return []providertelemetry.Observation{{Provider: credential.Provider, KeyID: credential.KeyID, Kind: providertelemetry.KindQuota, UsageUnit: "requests", Amount: "1000", Provenance: providertelemetry.ProvenanceProviderAPI, Certainty: providertelemetry.CertaintyExact, ObservedAt: observed, FreshUntil: observed.Add(time.Hour)}}, nil
		}),
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runner.Collect(context.Background(), providertelemetry.Credential{Provider: "cohere", KeyID: "key-a", Secret: []byte("secret")}, now)
	snapshot.Observations[0].Amount = "2000"
	if _, err := providertelemetry.MarshalControlledProjection(snapshot); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("mutated snapshot error = %v", err)
	}
	snapshot.SchemaVersion = 0
	if _, err := providertelemetry.MarshalControlledProjection(snapshot); err == nil {
		t.Fatal("projection accepted a snapshot without a schema version")
	}
}

func TestStaleOrIdentityMismatchedDataFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for name, observation := range map[string]providertelemetry.Observation{
		"stale":             {Provider: "cohere", KeyID: "key-a", Kind: providertelemetry.KindQuota, UsageUnit: "requests", Amount: "1000", Provenance: providertelemetry.ProvenanceProviderAPI, Certainty: providertelemetry.CertaintyExact, ObservedAt: now.Add(-2 * time.Hour), FreshUntil: now.Add(time.Hour)},
		"wrong key":         {Provider: "cohere", KeyID: "another-key", Kind: providertelemetry.KindQuota, UsageUnit: "requests", Amount: "1000", Provenance: providertelemetry.ProvenanceProviderAPI, Certainty: providertelemetry.CertaintyExact, ObservedAt: now, FreshUntil: now.Add(time.Hour)},
		"unversioned price": {Provider: "cohere", Kind: providertelemetry.KindPrice, DeploymentID: "dep", ModelReference: "cohere/model@rev", UsageUnit: "input_tokens", Currency: "USD", Amount: "0.1", Provenance: providertelemetry.ProvenanceProviderAPI, Certainty: providertelemetry.CertaintyExact, ObservedAt: now, FreshUntil: now.Add(time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			runner, err := providertelemetry.NewRunner(map[contract.ProviderSlug]providertelemetry.Collector{"cohere": collectorFunc(func(context.Context, providertelemetry.Credential, time.Time) ([]providertelemetry.Observation, error) {
				return []providertelemetry.Observation{observation}, nil
			})}, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			got := runner.Collect(context.Background(), providertelemetry.Credential{Provider: "cohere", KeyID: "key-a", Secret: []byte("secret")}, now).Observations[0]
			if got.Certainty != providertelemetry.CertaintyUnknown || got.UnknownReason != "invalid_provider_observation" {
				t.Fatalf("invalid data accepted: %#v", got)
			}
		})
	}
}
