package kaana

import (
	"context"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// The cache-write unit is emitted only where it is billable.
//
// An adapter reports what it measured: input tokens written to a prompt cache
// are a `cache_write_input_tokens` quantity, disjoint from `input_tokens`. Oxy
// rejects a settlement that carries a unit its price list for the line does not
// price (`unpriced_unit`, 422), so the split may reach the customer and the
// settlement only for a request whose granted price version prices the unit.
// For every other request the tokens go back into `input_tokens`, which is how
// every cache write was metered before the unit existed. That decision is made
// here, once, for every adapter, from a fact the caller supplies in the
// context; with none supplied the answer is "not priced", so a caller that
// knows nothing about prices changes nothing.

// CacheWritePriced reports whether the price list the request is granted
// against, for a model reference, carries a price for cache_write_input_tokens.
type CacheWritePriced func(contract.ModelReference) bool

type cacheWritePricedKey struct{}

// WithCacheWritePriced returns a context that tells the executor which routes
// of the request may report cache_write_input_tokens.
func WithCacheWritePriced(ctx context.Context, priced CacheWritePriced) context.Context {
	return context.WithValue(ctx, cacheWritePricedKey{}, priced)
}

func cacheWritePricedFrom(ctx context.Context) CacheWritePriced {
	priced, _ := ctx.Value(cacheWritePricedKey{}).(CacheWritePriced)
	return priced
}

// FoldCacheWrite returns units with any cache-write quantity folded into
// input_tokens unless priced is true. The input is not modified. Units that
// carry no cache write are returned as they came.
func FoldCacheWrite(units []contract.UsageQuantity, priced bool) []contract.UsageQuantity {
	if priced {
		return units
	}
	written := 0
	for _, quantity := range units {
		if quantity.Unit == contract.UnitCacheWriteInputTokens {
			written += quantity.Quantity
		}
	}
	if written == 0 {
		return units
	}
	folded := make([]contract.UsageQuantity, 0, len(units))
	hasInput := false
	for _, quantity := range units {
		switch quantity.Unit {
		case contract.UnitCacheWriteInputTokens:
			continue
		case contract.UnitInputTokens:
			quantity.Quantity += written
			hasInput = true
		}
		folded = append(folded, quantity)
	}
	if !hasInput {
		folded = append(folded, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: written})
	}
	return folded
}

// withheldCacheBreakpoints is the request a route is sent. A prompt-cache
// breakpoint makes the provider write the cache, which it bills above the plain
// input rate, so a route whose granted price version does not price
// cache_write_input_tokens is sent the request without them: it behaves exactly
// as it did before the marker existed, and nothing is written that the settlement
// could not price. A priced route gets the request as it came. The request is
// never modified, because a later route of the same request may be priced.
func withheldCacheBreakpoints(request *contract.Request, priced bool) *contract.Request {
	if priced {
		return request
	}
	withheld, _ := contract.LimitCacheBreakpoints(request, 0)
	return withheld
}
