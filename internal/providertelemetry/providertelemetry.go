// Package providertelemetry collects upstream economic facts for operators.
//
// The projection is deliberately separate from inference responses and from
// the published contract. It contains no customer amount and no credential
// material; Oxy may ingest it through a separately signed control-plane task.
package providertelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

type Provenance string

const (
	ProvenanceProviderAPI           Provenance = "provider_api"
	ProvenanceProviderResponse      Provenance = "provider_response"
	ProvenanceProviderDocumentation Provenance = "provider_documentation"
	ProvenanceOperator              Provenance = "operator"
)

type Certainty string

const (
	CertaintyExact     Certainty = "exact"
	CertaintyEstimated Certainty = "estimated"
	CertaintyUnknown   Certainty = "unknown"
)

type Kind string

const (
	KindBalance Kind = "balance"
	KindQuota   Kind = "quota"
	KindPrice   Kind = "price"
)

// Credential is supplied directly by Kaana's KMS-backed resolver. Secret is
// available only to the collector call and has no place in an Observation.
type Credential struct {
	Provider contract.ProviderSlug
	KeyID    string
	Secret   []byte
}

// Observation is one provider-owned upstream fact. Amount is a decimal string
// so collectors never pass monetary or quota arithmetic through float64.
// Unknown observations carry no amount or currency.
type Observation struct {
	Provider       contract.ProviderSlug `json:"provider"`
	KeyID          string                `json:"keyId,omitempty"`
	Kind           Kind                  `json:"kind"`
	DeploymentID   string                `json:"deploymentId,omitempty"`
	ModelReference string                `json:"modelReference,omitempty"`
	UsageUnit      string                `json:"usageUnit"`
	Currency       string                `json:"currency,omitempty"`
	Amount         string                `json:"amount,omitempty"`
	Provenance     Provenance            `json:"provenance"`
	Certainty      Certainty             `json:"certainty"`
	SourceVersion  string                `json:"sourceVersion,omitempty"`
	ObservedAt     time.Time             `json:"observedAt"`
	FreshUntil     time.Time             `json:"freshUntil"`
	UnknownReason  string                `json:"unknownReason,omitempty"`
}

// Snapshot is the complete operator projection from one collection pass.
type Snapshot struct {
	IssuedAt     time.Time     `json:"issuedAt"`
	Observations []Observation `json:"observations"`
}

// MarshalControlledProjection produces the non-secret payload a separately
// authenticated operator channel may deliver to Oxy. Keeping serialization in
// this package prevents a caller from accidentally marshalling Credential.
func MarshalControlledProjection(snapshot Snapshot) ([]byte, error) {
	for _, observation := range snapshot.Observations {
		if observation.Certainty == CertaintyUnknown && observation.Amount != "" {
			return nil, errors.New("provider telemetry: unknown observation carries an amount")
		}
	}
	return json.Marshal(snapshot)
}

// Collector implements one provider's official pricing, balance or quota API.
// An implementation must use provider-owned fixtures in its tests.
type Collector interface {
	Collect(context.Context, Credential, time.Time) ([]Observation, error)
}

type Runner struct {
	collectors map[contract.ProviderSlug]Collector
	maxAge     time.Duration
}

func NewRunner(collectors map[contract.ProviderSlug]Collector, maxAge time.Duration) (*Runner, error) {
	if len(collectors) == 0 {
		return nil, errors.New("provider telemetry: no collectors")
	}
	if maxAge <= 0 {
		return nil, errors.New("provider telemetry: freshness horizon must be positive")
	}
	owned := make(map[contract.ProviderSlug]Collector, len(collectors))
	for provider, collector := range collectors {
		if provider == "" || collector == nil {
			return nil, errors.New("provider telemetry: every collector needs a provider and implementation")
		}
		owned[provider] = collector
	}
	return &Runner{collectors: owned, maxAge: maxAge}, nil
}

// Collect executes the exact provider collector. Failure becomes an explicit
// unknown observation: an unavailable API can never look like zero capacity.
func (r *Runner) Collect(ctx context.Context, credential Credential, now time.Time) Snapshot {
	unknown := func(reason string) Snapshot {
		return Snapshot{IssuedAt: now, Observations: []Observation{{
			Provider: credential.Provider, KeyID: credential.KeyID, Kind: KindBalance,
			UsageUnit: "unknown", Provenance: ProvenanceProviderAPI,
			Certainty: CertaintyUnknown, ObservedAt: now, FreshUntil: now,
			UnknownReason: reason,
		}}}
	}
	if credential.Provider == "" || credential.KeyID == "" || len(credential.Secret) == 0 {
		return unknown("credential_unavailable")
	}
	collector, found := r.collectors[credential.Provider]
	if !found {
		return unknown("collector_unavailable")
	}
	observations, err := collector.Collect(ctx, credential, now)
	if err != nil {
		return unknown("provider_unavailable")
	}
	for index := range observations {
		observation := &observations[index]
		if err := validate(*observation, credential, now, r.maxAge); err != nil {
			return unknown("invalid_provider_observation")
		}
	}
	sort.Slice(observations, func(a, b int) bool {
		left, right := observations[a], observations[b]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.DeploymentID != right.DeploymentID {
			return left.DeploymentID < right.DeploymentID
		}
		if left.ModelReference != right.ModelReference {
			return left.ModelReference < right.ModelReference
		}
		return left.UsageUnit < right.UsageUnit
	})
	return Snapshot{IssuedAt: now, Observations: observations}
}

var decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func validate(observation Observation, credential Credential, now time.Time, maxAge time.Duration) error {
	if observation.Provider != credential.Provider || (observation.KeyID != "" && observation.KeyID != credential.KeyID) {
		return errors.New("identity mismatch")
	}
	if observation.Kind != KindBalance && observation.Kind != KindQuota && observation.Kind != KindPrice {
		return errors.New("invalid kind")
	}
	if observation.UsageUnit == "" {
		return errors.New("missing usage unit")
	}
	if observation.Kind != KindPrice && observation.KeyID == "" {
		return errors.New("capacity observation has no key id")
	}
	if observation.Provenance != ProvenanceProviderAPI && observation.Provenance != ProvenanceProviderResponse &&
		observation.Provenance != ProvenanceProviderDocumentation && observation.Provenance != ProvenanceOperator {
		return errors.New("invalid provenance")
	}
	if observation.ObservedAt.IsZero() || observation.ObservedAt.After(now) || now.Sub(observation.ObservedAt) > maxAge ||
		observation.FreshUntil.Before(now) || observation.FreshUntil.Before(observation.ObservedAt) {
		return errors.New("stale observation")
	}
	if observation.Certainty == CertaintyUnknown {
		if observation.Amount != "" || observation.Currency != "" || observation.UnknownReason == "" {
			return errors.New("invalid unknown observation")
		}
		return nil
	}
	if observation.Certainty != CertaintyExact && observation.Certainty != CertaintyEstimated {
		return errors.New("invalid certainty")
	}
	if !decimalPattern.MatchString(observation.Amount) || observation.UnknownReason != "" {
		return errors.New("invalid measured amount")
	}
	if observation.Currency != "" && !currencyPattern.MatchString(observation.Currency) {
		return fmt.Errorf("invalid currency %q", observation.Currency)
	}
	if observation.Kind == KindPrice && (observation.SourceVersion == "" || observation.DeploymentID == "" || observation.ModelReference == "") {
		return errors.New("unversioned price observation")
	}
	return nil
}
