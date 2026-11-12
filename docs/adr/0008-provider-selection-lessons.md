# ADR 0008 — Provider selection: what Kaana adopts from OpenRouter, and what it refuses

- Status: accepted for decisions 1-5 (Kaana-only, no contract change; 1 and 3 are implemented, 4 is deferred); decision 6
  is proposed and needs Oxy's half before any contract change.
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
4. **Cache affinity is deferred.** OpenRouter keeps a conversation on the
   provider whose prompt cache is warm. Kaana has no key to hold a conversation
   to: a grant carries none, and deriving one from the prompt would persist
   customer data. It waits for an opaque, Oxy-signed affinity value in the
   grant (a contract widening, so decision 6's route).
5. **Zero retention means zero retention.** OpenRouter's `zdr: true` also admits
   "Safety Retention" endpoints (retained only to detect abuse) unless the
   account or guardrail is set to ZDR only. Kaana's claim of zero retention is
   true only while that account setting holds, so the publisher's use of
   `/endpoints/zdr` is recorded as depending on it
   ([inventory](../inventory.md)), and the setting is part of the rollout check.
6. **Proposed, not yet decided: caller preferences that only narrow.** An
   optional public `provider` object (`only`, `ignore`, `sort`, `max_price`,
   `require_parameters`, `allow_fallbacks`, `quantizations`, `zdr`) that travels
   in `GrantRequestFacts` and that Oxy intersects with Console policy. Kaana may
   narrow with it and never widen. This widens the contract and needs Oxy's half
   first; it ships only after both ADRs are accepted.

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
- Quantization is not modelled. OpenRouter publishes it per endpoint; adding it
  waits for decision 6, because without a caller preference nothing would use it.
