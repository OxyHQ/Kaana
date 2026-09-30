package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

// publicationEvidenceReader is what the PostgreSQL store offers; an interface
// so the conversion is tested without a database.
type publicationEvidenceReader interface {
	ReadPublicationEvidence(context.Context, []contract.ProviderSlug, time.Time) (credentialstore.PublicationEvidence, error)
}

// storeEvidence adapts the credential store's rows to the publisher's
// evidence, so neither package imports the other.
type storeEvidence struct {
	reader publicationEvidenceReader
}

func (s storeEvidence) PublicationEvidence(ctx context.Context, providers []contract.ProviderSlug, since time.Time) (publisher.Evidence, error) {
	read, err := s.reader.ReadPublicationEvidence(ctx, providers, since)
	if err != nil {
		return publisher.Evidence{}, err
	}
	return convertEvidence(read), nil
}

func convertEvidence(read credentialstore.PublicationEvidence) publisher.Evidence {
	evidence := publisher.Evidence{
		Keys:     make(map[contract.ProviderSlug][]publisher.KeyEvidence),
		Bindings: make(map[contract.DeploymentID]publisher.KeyBinding, len(read.Bindings)),
		Failures: make([]publisher.FailureStreak, 0, len(read.Failures)),
	}
	for _, key := range read.Keys {
		converted := publisher.KeyEvidence{
			KeyID:        key.KeyID,
			Retirement:   string(key.Runtime.Reason),
			RetiredUntil: key.Runtime.RetiredUntil,
		}
		for _, observation := range key.Capacity {
			converted.Capacity = append(converted.Capacity, publisher.CapacityEvidence{
				Kind: observation.Kind, Window: observation.QuotaWindow,
				ObservedAt: observation.ObservedAt, FreshUntil: observation.FreshUntil,
				Empty: observation.Empty(), ExpiresAt: observation.ExpiresAt,
			})
		}
		evidence.Keys[key.Provider] = append(evidence.Keys[key.Provider], converted)
	}
	for _, binding := range read.Bindings {
		evidence.Bindings[binding.DeploymentID] = publisher.KeyBinding{Provider: binding.Provider, KeyID: binding.KeyID}
	}
	for _, streak := range read.Failures {
		evidence.Failures = append(evidence.Failures, publisher.FailureStreak{
			DeploymentID: streak.DeploymentID, Provider: streak.Provider, KeyID: streak.KeyID,
			Code: streak.FailureCode, Count: streak.Failures, First: streak.FirstAt, Last: streak.LastAt,
		})
	}
	return evidence
}

// withholdPolicyFromEnv reads the withholding policy. Every variable is
// optional and defaults conservatively; an unparseable value refuses to start
// rather than falling back, for the reason intervalFromEnv gives.
func withholdPolicyFromEnv(getenv func(string) string) (publisher.WithholdPolicy, error) {
	policy := publisher.DefaultWithholdPolicy()

	switch mode := strings.TrimSpace(getenv("KAANA_PUBLISHER_WITHHOLDING")); mode {
	case "", "enforce":
	case "report":
		policy.ReportOnly = true
	default:
		return publisher.WithholdPolicy{}, fmt.Errorf("KAANA_PUBLISHER_WITHHOLDING is %q; it is enforce (the default) or report", mode)
	}

	if value := strings.TrimSpace(getenv("KAANA_PUBLISHER_WITHHOLD_FAILURES")); value != "" {
		failures, err := strconv.Atoi(value)
		if err != nil {
			return publisher.WithholdPolicy{}, fmt.Errorf("KAANA_PUBLISHER_WITHHOLD_FAILURES is %q, which is not a whole number", value)
		}
		policy.Failures = failures
	}
	for _, duration := range []struct {
		name   string
		target *time.Duration
	}{
		{"KAANA_PUBLISHER_WITHHOLD_FAILURE_SPAN", &policy.FailureSpan},
		{"KAANA_PUBLISHER_WITHHOLD_MIN", &policy.MinQuarantine},
		{"KAANA_PUBLISHER_WITHHOLD_MAX", &policy.MaxQuarantine},
		{"KAANA_PUBLISHER_WITHHOLD_LOOKBACK", &policy.Lookback},
	} {
		value := strings.TrimSpace(getenv(duration.name))
		if value == "" {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return publisher.WithholdPolicy{}, fmt.Errorf("%s is %q, which is not a duration: %w", duration.name, value, err)
		}
		*duration.target = parsed
	}
	if err := policy.Validate(); err != nil {
		return publisher.WithholdPolicy{}, err
	}
	return policy, nil
}
