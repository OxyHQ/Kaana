package contract

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Identifiers Relay carries but never owns.
//
// ADR 0006: the data plane stores Oxy identifiers as immutable opaque strings.
// They are distinct Go types so that an account id cannot be passed where an
// application id is expected — the same separation the contract makes with zod
// brands, which are erased at runtime and therefore buy nothing on this side of
// the wire unless the types are restated.
type (
	// AccountID is the Oxy account that owns the workload and is financially
	// responsible for it. Relay never parses it, joins on it, or writes a row
	// keyed by it.
	AccountID string
	// ApplicationID is the Oxy Application consuming inference.
	ApplicationID string
	// CredentialID is the Oxy ApplicationCredential that authenticated the
	// request. Relay never sees the credential's secret.
	CredentialID string
	// UserID is the optional delegated end user. Attribution only: it is never
	// a billing principal and never an access-control principal.
	UserID string
	// RequestID correlates the Oxy edge, Relay, the ledger and the customer
	// receipt. It arrives on the envelope; Relay does not allocate it.
	RequestID string
	// GenerationID names a generation that can be looked up later. Relay
	// allocates it when a request produces one.
	GenerationID string
	// IdempotencyKey is the caller's deduplication key.
	IdempotencyKey string
)

// Catalogue references. Their grammar is public contract, because these are the
// strings a customer types.
type (
	// ModelID is `<publisher>/<model>` — a model line, not a revision.
	ModelID string
	// ModelReference is `<publisher>/<model>` or `<publisher>/<model>@<revision>`.
	ModelReference ModelID
	// RoutingProfileSlug names a strategy for choosing among routes. It can
	// never be written in the shape of a model id.
	RoutingProfileSlug string
	// ProviderSlug identifies who runs the weights for a route.
	ProviderSlug string
	// DeploymentID names one concrete servable route. Opaque to customers.
	DeploymentID string
	// Region is a provider-defined region identifier.
	Region string
	// Timestamp is an instant as an ISO 8601 string in UTC.
	Timestamp string
)

// The patterns below are the load-bearing ones: Relay validates against them,
// so a change to any of them changes what Relay accepts or emits.
// contract_test.go asserts each source string equals the published schema's, so
// a grammar change upstream is a failing test rather than a silent divergence.
var (
	modelReferencePattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?\/[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?(?:@[a-zA-Z0-9](?:[a-zA-Z0-9._-]*[a-zA-Z0-9])?)?$`)
	modelIDPattern            = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?\/[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)
	providerSlugPattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)
	routingProfileSlugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)
	regionPattern             = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)
)

// Valid reports whether the reference is a well-formed model reference.
func (r ModelReference) Valid() bool {
	return len(r) <= 194 && modelReferencePattern.MatchString(string(r))
}

// Pinned reports whether the reference names an immutable revision.
//
// The distinction is load-bearing rather than cosmetic: a request that pinned a
// revision asked for exactly those weights and is served or refused, never
// substituted.
func (r ModelReference) Pinned() bool { return strings.Contains(string(r), "@") }

// ModelID returns the unpinned model line a reference belongs to.
func (r ModelReference) ModelID() ModelID {
	if index := strings.Index(string(r), "@"); index >= 0 {
		return ModelID(r[:index])
	}
	return ModelID(r)
}

// Valid reports whether the id is a well-formed `<publisher>/<model>`.
func (m ModelID) Valid() bool { return len(m) <= 129 && modelIDPattern.MatchString(string(m)) }

// Valid reports whether the slug is a well-formed provider slug.
func (p ProviderSlug) Valid() bool {
	return len(p) > 0 && len(p) <= 64 && providerSlugPattern.MatchString(string(p))
}

// Valid reports whether the slug is a well-formed routing-profile slug.
func (s RoutingProfileSlug) Valid() bool {
	return len(s) > 0 && len(s) <= 64 && routingProfileSlugPattern.MatchString(string(s))
}

// Valid reports whether the region is a well-formed region identifier.
func (r Region) Valid() bool {
	return len(r) > 0 && len(r) <= 64 && regionPattern.MatchString(string(r))
}

// timestampLayout is the one spelling this contract admits: UTC, millisecond
// precision, trailing `Z`. Two records describing the same moment must compare
// and sort as equal, which a mix of offsets and precisions would not.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// NewTimestamp renders an instant in the contract's canonical spelling.
func NewTimestamp(at time.Time) Timestamp {
	return Timestamp(at.UTC().Format(timestampLayout))
}

// Time parses a timestamp back to an instant.
func (t Timestamp) Time() (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, string(t))
	if err != nil {
		return time.Time{}, fmt.Errorf("contract: %q is not an ISO 8601 instant: %w", string(t), err)
	}
	return parsed, nil
}
