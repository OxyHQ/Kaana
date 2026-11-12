# The two lanes

Kaana has two ways in. They share one executor, one inventory and one key store,
and differ in **who decides which route a request takes**. This page is the one
statement of that split; `README.md`, `architecture.md`, `routing.md` and
`inference-rules.md` link here instead of retelling it.

## The public lane

`https://kaana.ai/v1`: what customers, the SDK and tools such as Claude Code call.
`/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/embeddings`,
`/v1/audio/*`, `/v1/images/generations`, `/v1/decisions`, `/v1/realtime`,
`/v1/models`, `/v1/power-levels` and `/v1/generations`
(`internal/publicapi/server.go`).

```text
caller -> Kaana: request with an oxy_sk_ key
Kaana  -> Oxy:   content-free facts + hash of the body   (authorize)
Oxy    -> Kaana: signed grant: principal, policy, steps, admitted model lines
Kaana:           resolves the grant into ordered routes, executes, fails over
Kaana  -> Oxy:   one settlement per grant: units, never an amount   (outbox)
```

- Kaana never interprets the credential. It forwards it to Oxy, keeps only a
  digest for a per-credential rate limit, and refuses to run anything Oxy did not
  grant (`internal/publicapi/authorize.go`, `pipeline.go`).
- The **grant** bounds the request: which model lines, which providers and
  regions, a funding ceiling, zero retention, the hold. Kaana resolves it into
  routes (`resolveGrant`, `routes.go`): the model line's deployments that the
  grant admits and whose key pool has a key inside the ceiling, ordered by key
  funding tier (free, credits, paid), then the grant's line order, then the
  caller's `provider` preferences and cache affinity, then recent latency, then
  inventory position ([ADR 0008](adr/0008-provider-selection-lessons.md)). A
  preference can only narrow what the grant admitted.
- Settlement is written once per grant, whatever the outcome, in the same
  transaction as Kaana's own upstream-cost rows, and delivered by a worker
  (`internal/publicapi/settle.go`, table `inference_settlement_outbox`).

## The signed lane

`POST /internal/v1/inference`, `GET /internal/v1/realtime`,
`GET /internal/v1/health`, `GET /internal/v1/models`, the deployment query and
the provider-telemetry feeds (`internal/httpapi`). Oxy's edge and first-party
services (for example Alia's voice calls) use it. The caller is a signed Oxy
service, not a customer.

- Oxy resolves the model or power level **before** signing. The envelope carries
  an ordered `authorizedRoutes` list of exact `deploymentId` values.
- Kaana verifies every signed identity against one inventory snapshot and
  attempts the list **exactly as received**. It never reorders it by health,
  price or inventory position, never adds a route and never interprets the
  routing-policy reference ([routing.md](routing.md)).
- The usage record rides the stream as a `usage_report` event; Oxy settles from it.

## Who decides what

| | Public lane | Signed lane |
|---|---|---|
| Who the caller is | Oxy, from the forwarded credential | Oxy's edge, by signing the envelope |
| Which model lines may serve | Oxy, in the grant | Oxy, in the envelope |
| Power level to model lines | Kaana, inside the lines the grant admits | Oxy, before signing |
| Route order | Kaana, inside the grant | Oxy; Kaana keeps it exactly |
| Which key serves a deployment | Kaana: its provider pool, cheapest funding first | the same |
| Same-route retry and failover, before the first output | Kaana, inside the routes | the same |
| Spend hold, price, ledger | Oxy today; [ADR 0010](adr/0010-kaana-sells-and-invoices.md) proposes Kaana | Oxy |
| Settlement | Kaana's outbox to Oxy, units only | the `usage_report` event |

What never differs: Kaana does not attempt a route that no signed envelope or
grant authorized, a pinned model revision is never substituted, and a failover
after output has reached the caller is refused.

## What is shared and what is per process

Provider keys, their funding tier and their recovery leases are in PostgreSQL and
shared by every task. **Breakers, latency and throughput readings, cache
affinity, the per-credential rate limiter and BYOK throttles are per process.**
Production runs two `kaana` tasks, so each task has its own view of them: one can
have a breaker open while the other keeps trying the deployment, and the
effective per-credential rate is twice the configured one. Nothing that must be
exact (a balance, a hold) may live in that state.

## Where to go next

- Failover, retries, breakers: [routing.md](routing.md).
- Key pools and funding: [key-pools.md](key-pools.md).
- The inventory a route is resolved against: [inventory.md](inventory.md).
- Caller-facing provider preferences: [ADR 0008](adr/0008-provider-selection-lessons.md).
- The boundary with Oxy and its proposed change: [architecture.md](architecture.md),
  [ADR 0010](adr/0010-kaana-sells-and-invoices.md).
