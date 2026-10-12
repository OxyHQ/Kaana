# ADR 0001 — Kaana owns provider keys, costs and routing; Oxy owns customers and limits

- Status: accepted (phase 1 implemented here; phase 2 planned)
- Date: 2026-10-12
- Decided by: the owner of Oxy
- Counterpart: [Oxy ADR 0038](https://github.com/OxyHQ/oxy/blob/main/docs/adr/0038-kaana-owns-provider-keys-costs-and-routing.md),
  which supersedes the parts of Oxy ADRs 0027, 0036 and 0037 this changes.

## Context

Oxy Console is to Oxy what Google Cloud Console is to Google: developer
accounts, their API keys and application IDs, customer credits, billing,
quotas and limits — what Oxy CHARGES. Kaana is everything AI: it holds every
provider key and must be able to use any key of any provider as it sees fit.

Until this decision the code said the opposite. A platform deployment ran on
ONE exact key (its binding, else its provider's only key); a provider with two
keys and no binding was unroutable; "Kaana never picks among several keys"; a
second key's capacity reached a request only as another Oxy-signed deployment.
Oxy held provider prices, key economics and the order (Oxy ADRs 0027, 0036,
0037). With Anthropic holding two promotional keys, the second key could only
be used by duplicating thirteen deployments and teaching Oxy to order them.

## Decision

1. **Kaana owns provider keys, provider costs and routing.** What providers
   charge Kaana, the economics of each key (free, promotional or trial credit,
   prepaid, paid; which funding account) and the order in which keys and
   providers are tried live in Kaana.
2. **Oxy owns customers and limits.** Oxy tells Kaana what the customer asked
   for and the limits — a model or a power level, a maximum price per
   request, data retention and privacy, any funding ceiling — and Kaana
   chooses within them. Kaana returns the real cost and Oxy bills from Kaana's
   receipt. **Kaana never spends beyond what Oxy authorized.**
3. **The order is always free → credits → paid, for the SAME requested
   model, across all providers and all keys.** If some provider serves the
   requested model for free, serve it there; otherwise where Oxy holds credits
   (promotional or credit grants, prepaid); only then a paid one. Kaana never
   substitutes a different model because it is cheaper or free. Hard
   constraints always win over the order: the route must accept the request's
   parameters, and the data-retention/privacy policy, the price ceiling and any
   funding ceiling Oxy sends (Alia Pro and Ultra: credits only) are never
   relaxed to reach a cheaper key. Within one economic tier: cheaper first,
   then the existing latency and health signals.
4. **Power levels move to Kaana (phase 2).** `auto`, `instant`, `medium`,
   `high`, `xhigh`, `pro` and `ultra` are resolved by Kaana, which owns which
   models belong to each level. A request for a LEVEL means any model of that
   level, so the order of decision 3 applies across every model of the level
   and their providers and keys. A request for a CONCRETE model keeps the strict
   rule: that model only, free → credits → paid across its providers and keys.

## Phase 1 — implemented in this repository

- **A deployment is (provider, upstream model), executed on its provider's KEY
  POOL** (`provider.Registry.ResolveExecution`, `key-pools.md`): every enabled
  key, ordered by stated funding — free, promotional/trial, prepaid, paid,
  unstated — then position. Funding is derived from the key row's class and its
  protected description's capacity category; when both are stated the dearer
  reading wins (`internal/provider/funding.go`).
- **Unstated is spent last.** A key nobody described might be paid; spending
  it before a key stated paid would invent a saving, and admitting it under a
  ceiling could spend money Oxy never authorized. It is still not REPORTED as
  paid: telemetry does not invent an economic fact.
- **Inside a request**, a key that is spent (`provider_billing_refused`, a
  402, Anthropic `billing_error`) or refused (401/403 credential) is retired
  and the same attempt moves to the next key. A key failing transiently before
  any output (throttle, overload, timeout, 5xx, transport) retires nothing, and
  the same request's retry starts on another key. A request fault never moves.
  Moving keys keeps the deployment and emits no `route_switch`.
- **A retired key comes back** through the existing durable retirement and
  one-request recovery lease: when its retirement expires, the first real
  request that reaches it in pool order proves recovery.
- **Exact bindings are operator pins.** A pinned deployment uses only its key;
  pins are optional and are removed with the audited, compare-and-delete
  `kaana-credentials unpin-deployment` (migration `0021`). The "two or more
  keys and no binding → unroutable" rule is gone, with the startup and reload
  gate's dependence on it: the gate now refuses only a generation whose served
  providers hold no key, and warns about unresolvable deployments under
  `deployments without a platform credential are unroutable`.
- **Publication withholds a deployment only when no key of its pool can serve
  it** (`internal/publisher/withholding.go`), until the earliest one returns.
- **A funding ceiling** (`KeyPool.WithinFunding`) bounds a walk to keys whose
  stated tier is at or below it. The signed field that carries it, and the
  usage report's served funding tier, are contract fields (`@oxy.so/contracts`
  4.12.0, contract set 3.8.0, published from Oxy); Kaana adopts them in a
  follow-up change once that package is published (step 2 below).

### Rollout order

1. This change: pool routing, unpin, withholding. Safe alone: every provider
   but Anthropic holds one enabled key, and Anthropic's second key stays
   disabled until step 4.
2. Oxy publishes `@oxy.so/contracts` 4.12.0; Kaana pins it and honours
   `authorizedRoutes[].maxFundingTier` and reports `usage.fundingTier`.
3. Oxy deploys ADR 0038 phase 1: a multi-key provider's funding is the best
   AVAILABLE tier among its enabled keys, and a route Alia's Pro/Ultra lane
   admits on promotional funding is signed with `maxFundingTier: "credit"`.
4. Apply migration 0021, remove the 13 `dep_anthropic_*` pins, describe and
   enable the second Anthropic key (`operating.md`, "Moving a pinned provider
   onto its pool").

**Invariant until step 2 is deployed:** no provider may hold a paid or
unstated key beside the credit keys a credit-only route relies on, because no
request can yet carry the ceiling that keeps a walk off it.

## Phase 2 — planned, not implemented

Owned by Oxy ADR 0038, "Phase 2", which lists what moves, the contract
changes, the migration and how billing stays correct. In short: Oxy stops
choosing deployments. It sends the requested model OR level plus the limits
and policy; Kaana resolves the level, finds every deployment of the eligible
models, applies the hard constraints, and orders all (provider, key) pairs
free → credits → paid, cheaper first within a tier, then health. Provider
prices (today reviewed in Oxy's `kaanaReviewedProviderRoutes.ts` and the list
prices Kaana publishes) become Kaana's cost model; Oxy's price list stays what
it CHARGES.

## Open conflict to resolve before phase 2's power-level step

The owner's product fact that "in Alia end users pick only levels, never
concrete models" contradicts Alia's AGENTS.md and ADR 0012, which make the
picker real `publisher/model` catalogue models with no modes or profiles. It
does not affect phase 1. It must be settled before Kaana resolves levels for
Alia.

## Consequences

- One key spent no longer takes a provider's deployments out of publication
  or serving while another key of that provider can serve.
- Oxy's scorecards can no longer assume a deployment is funded by exactly one
  key; ADR 0038 replaces that rule.
- The deployment breaker is still per deployment: three consecutive transient
  failures across a pool's keys open it for every key. A per-key breaker is a
  phase 2 refinement if measurements show it is needed.
- Serving does not read operator capacity evidence when walking (publication
  does): a key whose balance an operator recorded as zero is tried first and
  retired by its own refusal, one upstream call later.
