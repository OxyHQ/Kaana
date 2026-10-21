package providerconfig

import "slices"

// openRouterUpstreamOrders pins, per OpenRouter upstream model id, the
// zero-retention upstreams that may serve it, in the order OpenRouter tries
// them. A model listed here is never load-balanced and never falls back to an
// upstream outside its list; a model not listed keeps OpenRouter's own
// routing.
//
// # qwen/qwen3-embedding-8b
//
// Measured 2026-10-21 from Kaana's own timing line (docs/operating.md, "Where
// a public request's time goes"): Oxy's grant took 35-195 ms, the
// evidence writes 2-10 ms, Kaana's own work 0-350 ms (the most for large
// batches), and the upstream exchange the rest: up to 16 s to the first byte from OpenRouter, which load-balances
// an unpinned request by price over the model's three zero-retention
// endpoints (Nebius, DeepInfra, SiliconFlow). Pinning DeepInfra, then Nebius:
//
//   - tries DeepInfra's US endpoint first on every request, rather than
//     whichever endpoint the balancer drew;
//   - excludes SiliconFlow, which serves an fp8 quantization at four times
//     the price. A stored embedding is compared with every later one, so
//     vectors must come from the same weights; mixing quantizations makes a
//     stored vector drift from a query vector of the same text.
//
// The publisher withholds a listed model when none of its pinned upstreams is
// in OpenRouter's zero-retention list (internal/publisher/zdr.go), so a pin
// never publishes a route that cannot be served.
var openRouterUpstreamOrders = map[string][]string{
	"qwen/qwen3-embedding-8b": {"deepinfra", "nebius"},
}

// OpenRouterUpstreamOrder returns the pinned upstream order for an OpenRouter
// upstream model id, or nil when OpenRouter routes it.
func OpenRouterUpstreamOrder(upstreamModelID string) []string {
	return slices.Clone(openRouterUpstreamOrders[upstreamModelID])
}
