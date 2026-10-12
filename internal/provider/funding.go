package provider

import "fmt"

// Funding is what spending on one platform key costs, as the operator stated
// it, at the grain Kaana orders a provider's key pool by.
//
// It is DERIVED from two stated facts and never inferred from traffic: the
// key's class on its credential row (`free`, `paid` or unstated) and the
// capacity category of its protected description (migration 0017: `trial`,
// `promotional`, `prepaid`, `paid`). Nothing observable separates a free-tier
// key from a funded one (`KeyClass`), so a key nobody described is
// FundingUnstated, never a cheaper class nobody proved.
type Funding string

const (
	// FundingFree costs no money at all: running it out is a rate limit, not a
	// bill. The key's row says `free`.
	FundingFree Funding = "free"
	// FundingPromotional is credit somebody granted: a promotional or trial
	// grant. Using it costs nothing until it is spent or expires.
	FundingPromotional Funding = "promotional"
	// FundingPrepaid is a balance Oxy already bought. Using it spends money
	// that was paid in advance, which is cheaper than nothing only in that it
	// is spent either way before it expires.
	FundingPrepaid Funding = "prepaid"
	// FundingPaid is ordinary metered spend: every request is a new charge.
	FundingPaid Funding = "paid"
	// FundingUnstated is a key whose economics nobody stated. It is NOT paid —
	// telemetry never invents an economic fact — but it is spent last and is
	// never admitted under a funding ceiling, because it might be.
	FundingUnstated Funding = ""
)

// FundingTier is the coarse economic order the owner's routing decision names:
// free, then credits, then paid. It is the vocabulary of a funding ceiling and
// of the receipt Oxy bills from.
type FundingTier string

const (
	FundingTierFree   FundingTier = "free"
	FundingTierCredit FundingTier = "credit"
	FundingTierPaid   FundingTier = "paid"
	// FundingTierNone is "no ceiling" when it bounds a walk, and "unstated"
	// when it describes a key.
	FundingTierNone FundingTier = ""
)

// Valid reports whether a tier is one of the three named tiers.
func (t FundingTier) Valid() bool {
	switch t {
	case FundingTierFree, FundingTierCredit, FundingTierPaid:
		return true
	}
	return false
}

func (t FundingTier) rank() int {
	switch t {
	case FundingTierFree:
		return 0
	case FundingTierCredit:
		return 1
	case FundingTierPaid:
		return 2
	}
	return 3
}

// Tier is the funding tier a key belongs to. Promotional and prepaid are both
// credits. An unstated key has no tier.
func (f Funding) Tier() FundingTier {
	switch f {
	case FundingFree:
		return FundingTierFree
	case FundingPromotional, FundingPrepaid:
		return FundingTierCredit
	case FundingPaid:
		return FundingTierPaid
	}
	return FundingTierNone
}

// Admits reports whether a walk bounded by this ceiling may spend a key of
// the given funding. No ceiling admits every key; a ceiling admits only a key
// whose STATED tier is at or below it, so an unstated key is never spent under
// a ceiling it might exceed.
func (t FundingTier) Admits(funding Funding) bool {
	if t == FundingTierNone {
		return true
	}
	tier := funding.Tier()
	return tier != FundingTierNone && tier.rank() <= t.rank()
}

// rank orders a pool: free, promotional, prepaid, paid, unstated. Within the
// credit tier a grant is spent before a prepaid balance — the grant costs
// nothing and expires; the balance is Oxy's money — and an unstated key comes
// after paid because the worst case of what it costs is paid and more.
func (f Funding) rank() int {
	switch f {
	case FundingFree:
		return 0
	case FundingPromotional:
		return 1
	case FundingPrepaid:
		return 2
	case FundingPaid:
		return 3
	}
	return 4
}

// FundingFor derives a key's funding from its row's class and its
// description's capacity category (empty when the key has no description).
//
// When both are stated the MORE EXPENSIVE reading wins. They are two
// statements by operators at different times; taking the cheaper one would
// spend a key ahead of its order, or admit it under a ceiling, on the
// strength of a statement the other one contradicts.
func FundingFor(class KeyClass, capacityCategory string) (Funding, error) {
	var fromClass Funding
	switch class {
	case KeyClassFree:
		fromClass = FundingFree
	case KeyClassPaid:
		fromClass = FundingPaid
	case KeyClassUnstated:
		fromClass = FundingUnstated
	default:
		return FundingUnstated, fmt.Errorf("provider: key class %q is not free, paid or unstated", class)
	}
	var fromCategory Funding
	switch capacityCategory {
	case "":
		return fromClass, nil
	case "trial", "promotional":
		fromCategory = FundingPromotional
	case "prepaid":
		fromCategory = FundingPrepaid
	case "paid":
		fromCategory = FundingPaid
	default:
		return FundingUnstated, fmt.Errorf("provider: capacity category %q is not trial, promotional, prepaid or paid", capacityCategory)
	}
	if fromClass == FundingUnstated || fromCategory.rank() >= fromClass.rank() {
		return fromCategory, nil
	}
	return fromClass, nil
}
