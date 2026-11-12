# ADR 0008 — Provider selection: what Kaana adopts from OpenRouter, and what it refuses

- Status: accepted. Decisions 1, 3, 4 and 6 are implemented; decision 5
  (zero retention) is recorded as a dependency and still needs the operator's
  call on Safety Retention.
- Date: 2026-11-12
- Source: OpenRouter's provider-selection, traffic-routing, model-fallbacks and
  zero-data-retention guides, read 2026-11-12.

## Context

OpenRouter lets a caller steer provider choice per request (`provider.order`,
`only`, `ignore`, `allow_fallbacks`, `sort`, `preferred_min_throughput`,
`preferred_max_latency`, `max_price`, `quantizations`, `require_parameters`,
`zdr`, `data_collection`) and, by default, spreads traffic by measured price,
throughput, time to first token, 429 rate and reliability. Kaana's shape is
different on purpose: Oxy signs the routes and the policy, Kaana executes
(`docs/routing.md`, "Rules a reviewer applies"). Kaana's public wire has no
`provider` object and refuses unknown fields (`internal/publicapi/body.go`).

Measured against that, the gaps worth closing are the ones that make Kaana pick
a route that cannot serve the request, ignore a limit Oxy already granted, or
leave measured evidence unused. The gaps that would let a caller widen what Oxy
authorized, or that would make ordering non-deterministic on the signed lane,
are not gaps.

## Decision

1. **Selection is capability-aware.** The grant lane drops a deployment whose
   published `acceptedParameters` refuse a control the request carries, instead
   of attempting it and ending the request with a terminal `invalid_request`
   while another admitted route could serve it. A deployment with no such
   statement (`nil`) is kept, as today. If no admitted deployment accepts the
   control, resolution falls back to the old behaviour so the attempt refuses
   with the exact parameter named, never a bare "no route". The same applies to
   a deployment's observed `contextTokens` and `maxOutputTokens`: a route
   carries them (`provider.Route`), an input past the window or an output limit
   above the deployment's own skips it, and the estimate being rough means only
   a clear excess does.
2. **`maxPricePerRequest` stays Oxy's.** OpenRouter's `max_price` blocks on
   price, but Kaana never prices (`docs/cost.md`): Oxy applies the ceiling when
   it computes `admittedModelLines` and sizes the hold, and Kaana relays it.
   Enforcing it here would need a price Kaana is forbidden to hold.
3. **Measured performance breaks ties, never excludes.** Within one funding tier
   and one model line, the grant lane orders by recent per-deployment time to
   first output over a rolling window, then inventory position. This is what
   ADR 0001 decision 3 already promised ("then existing latency and health
   signals"). It follows OpenRouter's rule that a performance preference "should
   never prevent your request from being executed": a slow route moves later, it
   is not removed. The signed `authorizedRoutes` lane is untouched: it is never
   reordered (`docs/routing.md`).
4. **Cache affinity uses a caller-supplied opaque key.** OpenRouter keeps a
   conversation on the provider whose prompt cache is warm. Kaana does it from
   the caller's `prompt_cache_key` (decision 6), never from the prompt: the
   deployment that last served a key is preferred for 10 minutes, in this process
   only, bounded, while it is still in the grant's admitted set and in rotation.
   It never outranks the funding tier, the grant's line order or an explicit
   `provider.order`.
5. **Zero retention means zero retention.** OpenRouter's `zdr: true` also admits
   "Safety Retention" endpoints (retained only to detect abuse) unless the
   account or guardrail is set to ZDR only. Kaana's claim of zero retention is
   true only while that account setting holds, so the publisher's use of
   `/endpoints/zdr` is recorded as depending on it
   ([inventory](../inventory.md)), and the setting is part of the rollout check.
6. **Caller preferences that only narrow are accepted** (contract 8.4.0). An
   optional `provider` object on chat completions and responses, in
   OpenRouter's names so a client written for OpenRouter works: `order`, `only`,
   `ignore`, `allow_fallbacks`, `require_parameters`, `quantizations`, `sort`
   (`latency` or `throughput`), `preferred_max_latency` and
   `preferred_min_throughput` (seconds and tokens per second at p50). Plus
   `prompt_cache_key` / `promptCacheKey`, which makes decision 4 possible without
   Oxy: it is an opaque caller string, scoped by account and credential and
   hashed, so Kaana keeps no prompt and no id.
   - It stays inside Kaana. Oxy never sees it, so the signed grant and envelope
     do not change and Oxy needs no change: the object filters or reorders
     routes the grant already admitted, and a request it leaves with no route
     is `no_route_available`, never widened.
   - `only`, `ignore`, `quantizations` and `require_parameters` remove routes.
     `order`, `sort`, the two `preferred_*` floors and the cache key reorder:
     funding tier and the grant's line order first, then `order`, then the
     floors (a route under a floor is tried later, never removed), then `sort`,
     then cache affinity, then Kaana's latency tie-break. `allow_fallbacks:
     false` keeps one provider (the listed ones when `order` is set).
   - Refused, not ignored (`.strict()`): `sort: "price"`, `max_price`, `zdr`,
     `data_collection`, percentile objects and `enforce_distillable_text`.
     Kaana never prices, and the data policy is Oxy's.
   - Provider names are Kaana provider slugs. They name what a caller asks for;
     no response, record or stream names one. This supersedes the earlier rule
     that no public shape names a provider, for these two requests only.
   - Throughput is measured like latency: served attempts feed a 5-minute
     moving average of output tokens per second after the first output.

## Refused

- **Weighted random traffic spreading.** It would break the signed order and
  `docs/determinism.md`.
- **Naming the serving provider in a public answer.** Neither OpenRouter's
  routing guide nor Kaana's contract does, and the public surface test forbids it.
- **Any field that lets a caller widen** providers, regions, funding or data
  policy beyond what Oxy signed.

## Consequences

- Routing now has a measured input, so the order of a grant-lane request can
  differ between two otherwise identical requests. `docs/determinism.md` says so.
- Per-process latency state is lost on restart, like the breaker state; a cold
  deployment starts neutral, not penalized.
- Quantization is observed. OpenRouter's zero-retention list states one level
  per endpoint, so a model's `observed.quantizations` is the set across its
  reachable endpoints; a direct provider states none and reads as `unknown`.
