package publicapi

import (
	"crypto/sha256"
	"math/big"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// providerPreferences is the `provider` object a caller may send on chat
// completions and responses (ADR 0008, decision 6). It only NARROWS what the
// grant admitted: it filters routes, reorders them, or trims fallbacks, and it
// can never add a provider, region, funding tier or price the grant did not.
//
// Only the controls Kaana can honour are decoded; decodeRequest refuses every
// other member (`max_price`, `zdr`, `data_collection`, `sort: "price"`),
// because pricing and data policy are Oxy's and Kaana never prices.
type providerPreferences struct {
	Order                  []string `json:"order,omitempty"`
	Only                   []string `json:"only,omitempty"`
	Ignore                 []string `json:"ignore,omitempty"`
	AllowFallbacks         *bool    `json:"allow_fallbacks,omitempty"`
	RequireParameters      *bool    `json:"require_parameters,omitempty"`
	Quantizations          []string `json:"quantizations,omitempty"`
	Sort                   *string  `json:"sort,omitempty"`
	PreferredMaxLatency    *float64 `json:"preferred_max_latency,omitempty"`
	PreferredMinThroughput *float64 `json:"preferred_min_throughput,omitempty"`
	// MaxPrice is a ceiling on the declared list price of a model line, in USD
	// per million tokens (see linePrice).
	MaxPrice *maxPrice `json:"max_price,omitempty"`
}

// maxPrice is the caller's ceiling. A component left out is not checked.
type maxPrice struct {
	Prompt     *float64 `json:"prompt,omitempty"`
	Completion *float64 `json:"completion,omitempty"`
}

var (
	providerSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	quantizationWords   = []string{"int4", "int8", "fp4", "mxfp4", "nvfp4", "fp6", "fp8", "mxfp8", "fp16", "bf16", "fp32", "unknown"}
)

const maxPreferenceProviders = 16

// validate refuses a preference the contract's schema would: the contract is
// the source, this is the data plane enforcing the same bounds.
func (p *providerPreferences) validate(requestID contract.RequestID) *contract.PublicError {
	if p == nil {
		return nil
	}
	for name, list := range map[string][]string{"provider.order": p.Order, "provider.only": p.Only, "provider.ignore": p.Ignore} {
		if list != nil && (len(list) == 0 || len(list) > maxPreferenceProviders) {
			return invalid(requestID, name, name+" names 1 to 16 providers")
		}
		for _, slug := range list {
			if len(slug) > 64 || !providerSlugPattern.MatchString(slug) {
				return invalid(requestID, name, name+" holds a provider slug: lowercase letters, digits, '.', '_' or '-'")
			}
		}
	}
	if p.Quantizations != nil && (len(p.Quantizations) == 0 || len(p.Quantizations) > len(quantizationWords)) {
		return invalid(requestID, "provider.quantizations", "provider.quantizations names 1 to 12 quantizations")
	}
	for _, word := range p.Quantizations {
		if !slices.Contains(quantizationWords, word) {
			return invalid(requestID, "provider.quantizations", "provider.quantizations holds "+word+", which is not a quantization")
		}
	}
	if p.Sort != nil && *p.Sort != "price" && *p.Sort != "latency" && *p.Sort != "throughput" {
		return invalid(requestID, "provider.sort", "provider.sort is price, latency or throughput")
	}
	if ceiling := p.MaxPrice; ceiling != nil {
		if ceiling.Prompt == nil && ceiling.Completion == nil {
			return invalid(requestID, "provider.max_price", "provider.max_price names prompt, completion or both")
		}
		for name, value := range map[string]*float64{"prompt": ceiling.Prompt, "completion": ceiling.Completion} {
			if value != nil && (*value <= 0 || *value > 1_000_000) {
				return invalid(requestID, "provider.max_price."+name, "provider.max_price."+name+" is a positive USD price per million tokens up to 1000000")
			}
		}
	}
	if p.PreferredMaxLatency != nil && (*p.PreferredMaxLatency <= 0 || *p.PreferredMaxLatency > 600) {
		return invalid(requestID, "provider.preferred_max_latency", "provider.preferred_max_latency is a positive number of seconds up to 600")
	}
	if p.PreferredMinThroughput != nil && (*p.PreferredMinThroughput <= 0 || *p.PreferredMinThroughput > 100000) {
		return invalid(requestID, "provider.preferred_min_throughput", "provider.preferred_min_throughput is a positive number of tokens per second up to 100000")
	}
	return nil
}

// excludes applies the preferences that REMOVE a route: the provider allow and
// skip lists, the quantization filter and `require_parameters`. They are hard:
// a request they leave with no route is refused, never widened.
func (p *providerPreferences) excludes(route provider.Route, parameters []provider.RequestParameter) bool {
	if p == nil {
		return false
	}
	if len(p.Only) > 0 && !slices.Contains(p.Only, string(route.Provider)) {
		return true
	}
	if slices.Contains(p.Ignore, string(route.Provider)) {
		return true
	}
	if len(p.Quantizations) > 0 && !quantizationAdmitted(p.Quantizations, route.Quantizations) {
		return true
	}
	if p.RequireParameters != nil && *p.RequireParameters {
		if _, refused := route.UnacceptedParameter(parameters...); refused {
			return true
		}
	}
	return false
}

// quantizationAdmitted reads OpenRouter's rule: `fp4` covers `mxfp4` and
// `nvfp4`, `fp8` covers `mxfp8`, and a route that states no quantization is
// `unknown`.
func quantizationAdmitted(wanted []string, published []string) bool {
	if len(published) == 0 {
		published = []string{"unknown"}
	}
	for _, have := range published {
		for _, want := range wanted {
			if have == want || (want == "fp4" && (have == "mxfp4" || have == "nvfp4")) || (want == "fp8" && have == "mxfp8") {
				return true
			}
		}
	}
	return false
}

// providerRank is the position of a route's provider in `order`; a provider
// the caller did not list ranks after every listed one, and with no `order`
// every route ranks equal.
func (p *providerPreferences) providerRank(slug contract.ProviderSlug) int {
	if p == nil || len(p.Order) == 0 {
		return 0
	}
	if index := slices.Index(p.Order, string(slug)); index >= 0 {
		return index
	}
	return len(p.Order)
}

// keepsOneProvider reports `allow_fallbacks: false`.
func (p *providerPreferences) keepsOneProvider() bool {
	return p != nil && p.AllowFallbacks != nil && !*p.AllowFallbacks
}

// slow reports a route whose recent reading misses the caller's stated floor.
// It only demotes: a preference "should never prevent your request from being
// executed" (ADR 0008).
func (p *providerPreferences) slow(performance PerformanceReader, id contract.DeploymentID) bool {
	if p == nil || performance == nil {
		return false
	}
	if p.PreferredMaxLatency != nil {
		if observed, ok := performance.FirstOutput(id); ok && observed.Seconds() > *p.PreferredMaxLatency {
			return true
		}
	}
	if p.PreferredMinThroughput != nil {
		if observed, ok := performance.Throughput(id); ok && observed < *p.PreferredMinThroughput {
			return true
		}
	}
	return false
}

// affinityTTL is how long a deployment stays preferred for a cache key. The
// providers' own prompt caches live for minutes, so a longer hold would pin a
// conversation to a deployment whose cache is gone.
const affinityTTL = 10 * time.Minute

const maxAffinityEntries = 8192

// affinityKey scopes a caller's opaque `prompt_cache_key` to the caller, so
// two accounts that happen to send the same string never share a deployment
// choice. It is a hash: neither the key nor the caller's ids are kept.
func affinityKey(grant *contract.Grant, cacheKey string) [32]byte {
	hash := sha256.New()
	for _, part := range []string{string(grant.Principal.BillingAccountID), string(grant.Principal.CredentialID), cacheKey} {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	return key
}

type affinityEntry struct {
	deployment contract.DeploymentID
	at         time.Time
}

// affinityTable remembers, in this process only, which deployment last served a
// cache key. It holds no prompt and no caller id, is bounded, and is lost on
// restart like the breakers: a cold table just means no preference.
type affinityTable struct {
	mu      sync.Mutex
	entries map[[32]byte]affinityEntry
}

func newAffinityTable() *affinityTable {
	return &affinityTable{entries: make(map[[32]byte]affinityEntry)}
}

func (t *affinityTable) get(key [32]byte, at time.Time) (contract.DeploymentID, bool) {
	if t == nil {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[key]
	if !ok || at.Sub(entry.at) > affinityTTL {
		delete(t.entries, key)
		return "", false
	}
	return entry.deployment, true
}

func (t *affinityTable) set(key [32]byte, deployment contract.DeploymentID, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, known := t.entries[key]; !known && len(t.entries) >= maxAffinityEntries {
		for candidate, entry := range t.entries {
			if at.Sub(entry.at) > affinityTTL {
				delete(t.entries, candidate)
			}
		}
		if len(t.entries) >= maxAffinityEntries {
			var oldest [32]byte
			var oldestAt time.Time
			for candidate, entry := range t.entries {
				if oldestAt.IsZero() || entry.at.Before(oldestAt) {
					oldest, oldestAt = candidate, entry.at
				}
			}
			delete(t.entries, oldest)
		}
	}
	t.entries[key] = affinityEntry{deployment: deployment, at: at}
}

// validateRouting checks the routing preference and the prompt-cache key a
// request carries.
func validateRouting(requestID contract.RequestID, prefs *providerPreferences, cacheKey *string, cacheKeyParam string) *contract.PublicError {
	if failure := prefs.validate(requestID); failure != nil {
		return failure
	}
	if cacheKey != nil && (len(*cacheKey) == 0 || len(*cacheKey) > 256) {
		return invalid(requestID, cacheKeyParam, cacheKeyParam+" is 1 to 256 characters")
	}
	return nil
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// linePrice is the declared list price of one model line at the price version a
// grant admitted, in USD per million tokens. A component the catalogue does not
// state is nil: unknown is neither within a ceiling nor cheap.
//
// It is the price Oxy rates the grant at, so every route of the line costs the
// customer the same. That is why the price preferences decide which LINE serves
// a request and cannot reorder the routes within one.
type linePrice struct {
	Input  *big.Rat
	Output *big.Rat
}

// LinePricer reads declared list prices; *PublicCatalogue is the one.
type LinePricer interface {
	LinePrice(line contract.ModelID, priceVersionID string) linePrice
}

// within reports whether the price is under every component the caller set. A
// component the caller set that the catalogue does not state fails it.
func (p *providerPreferences) within(price linePrice) bool {
	if p == nil || p.MaxPrice == nil {
		return true
	}
	under := func(ceiling *float64, price *big.Rat) bool {
		if ceiling == nil {
			return true
		}
		if price == nil {
			return false
		}
		limit := new(big.Rat)
		if limit.SetFloat64(*ceiling) == nil {
			return false
		}
		return price.Cmp(limit) <= 0
	}
	return under(p.MaxPrice.Prompt, price.Input) && under(p.MaxPrice.Completion, price.Output)
}

// sortsByPrice reports `sort: "price"`.
func (p *providerPreferences) sortsByPrice() bool {
	return p != nil && p.Sort != nil && *p.Sort == "price"
}

// blended is the figure a line is ranked by under `sort: "price"`: its input
// plus its output price per million tokens. nil when either is not stated, which
// ranks after every priced line.
func (price linePrice) blended() *big.Rat {
	if price.Input == nil || price.Output == nil {
		return nil
	}
	return new(big.Rat).Add(price.Input, price.Output)
}

// hasCeiling reports a `max_price` the caller set.
func (p *providerPreferences) hasCeiling() bool {
	return p != nil && p.MaxPrice != nil
}
