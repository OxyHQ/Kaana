package nativeadmission

import (
	"math/big"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// CredentialFundingEvidence projects enabled keys from a successful local
// credential-economics read. It contains no key material or operator labels.
type CredentialFundingEvidence struct {
	Provider         contract.ProviderSlug
	KeyID            string
	CapacityCategory string
	Capacity         []FundingCapacity
}

type FundingCapacity struct {
	Kind, QuotaWindow, QuotaRemaining, BalancePicos string
	ObservedAt, FreshUntil, ExpiresAt               time.Time
}

// PromotionalCreditAvailable preserves the existing promotional admission gate.
// poolKeyIDs is the actual route's usable key set (including any exact binding),
// never all keys of an unrelated provider. An empty set admits nothing.
// readAt is the successful source read time, not the time a cache was accessed.
// This is admission evidence only: execution keeps its existing credit ceiling,
// which also permits free and prepaid keys; this function does not pin a key.
func PromotionalCreditAvailable(evidence []CredentialFundingEvidence, provider contract.ProviderSlug, poolKeyIDs []string, readAt, now time.Time) bool {
	if readAt.IsZero() || now.Before(readAt) || !now.Before(readAt.Add(6*time.Hour)) || len(poolKeyIDs) == 0 {
		return false
	}
	allowed := make(map[string]bool, len(poolKeyIDs))
	for _, id := range poolKeyIDs {
		allowed[id] = true
	}
	for _, key := range evidence {
		if key.Provider != provider || key.KeyID == "" || !allowed[key.KeyID] || key.CapacityCategory != "promotional" {
			continue
		}
		available := true
		for _, observation := range key.Capacity {
			// Stale quota/balance observations cease saying anything. A grant's
			// stated future expiry still bounds the successful read's lifetime.
			if observation.Kind == "expiry" && observation.ExpiresAt.After(readAt) && !now.Before(observation.ExpiresAt) {
				available = false
				break
			}
			if !observation.FreshUntil.IsZero() && !now.Before(observation.FreshUntil) {
				continue
			}
			switch observation.Kind {
			case "balance":
				available = !zeroFundingAmount(observation.BalancePicos)
			case "quota":
				available = !zeroFundingAmount(observation.QuotaRemaining)
			case "expiry":
				available = observation.ExpiresAt.IsZero() || now.Before(observation.ExpiresAt)
			}
			if !available {
				break
			}
		}
		if available {
			return true
		}
	}
	return false
}

func zeroFundingAmount(value string) bool {
	number, ok := new(big.Rat).SetString(value)
	return ok && number.Sign() == 0
}
