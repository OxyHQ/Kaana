<p align="center">
  <img src="docs/assets/kaana-logo.svg" alt="Kaana" width="640">
</p>

# Kaana

**Kaana is Oxy's inference platform.** One API executes requests across many
upstream providers and models, the same idea as OpenRouter, operated by Oxy for
Oxy's own products and for external customers. Its one canonical origin is
[`https://kaana.ai`](https://kaana.ai).

This repository is the Go data plane (`kaana`, its publisher and its operator
tools) plus the TypeScript workspace that defines the wire contract and the SDK:

| Workspace | What it is |
|---|---|
| `cmd/`, `internal/` | the Go data plane, deployed to ECS |
| `packages/contracts` | `@kaana.ai/contracts`, the Zod source of every request, response and signed shape |
| `packages/sdk` | `@kaana.ai/sdk`, the TypeScript client |
| `tools/contract` | generates `internal/contract/descriptor.json` and `packages/contracts/openapi.json` |
| `configs/` | the declared catalogue, deployment commercial facts, rate cards, model attribution |

Alia remains the agent runtime. Kaana executes model requests; conversations,
tools, memory and approvals belong to Alia.

## How a request reaches a provider

Kaana has two ways in, sharing one executor, one inventory and one key store. The
full split is in [docs/lanes.md](docs/lanes.md).

```text
customer / SDK / Claude Code -> https://kaana.ai/v1 -> Oxy grant -> Kaana routes, executes, settles
Oxy edge / first-party service -> /internal/v1/inference (signed) -> Kaana executes the signed order
```

- On the **public lane** Kaana asks Oxy for a signed grant, resolves it into
  ordered routes (key funding tier, the grant's line order, the caller's
  `provider` preferences, cache affinity, recent latency), executes with failover,
  and writes one settlement per grant.
- On the **signed lane** Oxy signs an ordered list of exact `deploymentId`
  values and Kaana attempts exactly that order.
- On both, Kaana picks the **key**: a deployment runs on its provider's pool,
  cheapest stated funding first, inside the ceiling the grant or envelope
  carries ([docs/key-pools.md](docs/key-pools.md)). It retries the same route on
  transient failures and fails over before the first output, never after.
- A request that names an exact model revision is served on exactly that revision
  or refused. It is **never quietly answered by a different model.**

New to Oxy inference? The concepts (exact model, power level, app default) and
the product-side request paths are in the
[Oxy inference developer guide](https://github.com/OxyHQ/oxy/blob/main/docs/inference/README.md).

## Who owns what

| Kaana (this repository) | Oxy |
|---|---|
| request normalization and the provider adapters | accounts, organizations, projects, applications |
| the public API, the grant resolution, routing execution, failover, streaming, cancellation | authentication of `oxy_sk_` keys and sessions, scopes, routing policy |
| model deployments, provider health, circuit breakers, the inventory | the Console, where applications and keys are managed |
| provider keys: KMS ciphertext custody, key pools, customer BYOK | provider-connection metadata and policy |
| the declared catalogue and its list prices | the spend hold, rating, ledger, receipts, payments and invoices |
| metering (exact units) and upstream provider cost | what a customer is charged |

Two notes on the right-hand column:

- It is how production works today. The owner has decided that **Kaana will sell
  and invoice, with Oxy keeping identity**; the proposal, its phases and its open
  questions are [ADR 0010](docs/adr/0010-kaana-sells-and-invoices.md). Nothing
  moves until it is accepted and its first phase ships.
- Upstream provider keys are never in environment variables, the repository or a
  product database: they are KMS ciphertext in Kaana's PostgreSQL store, including
  customer BYOK. Customer BYOK in source is not a production enablement claim; it
  stays ungrantable until its fee schedule and gates exist
  ([docs/customer-provider-credentials.md](docs/customer-provider-credentials.md)).

## Quick start

```bash
go build ./...
make check          # every CI gate, in CI's order
make help           # every target; make tools reports missing local tools
```

`make check` is the definition of done. `make test-integration` also needs a local
PostgreSQL in `KAANA_POSTGRES_TEST_URL`.

## Configuration

Every `KAANA_*` variable, what it is for and how a failure looks is in
[docs/operating.md](docs/operating.md). The ones a process cannot start without:

| Setting | Meaning |
|---|---|
| `KAANA_INVENTORY_PATH` | the deployment inventory snapshot (mounted by the publisher's output, never baked into the image; see [docs/inventory.md](docs/inventory.md)) |
| `KAANA_EDGE_PUBLIC_KEYS` | `kid:base64,...` Ed25519 **public** keys the Oxy edge signs with; not secret |
| `KAANA_PROVIDERS` | the provider slugs this process serves |
| `DATABASE_URL` | TLS PostgreSQL connection to Kaana's database |
| `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN` | the symmetric KMS key that encrypts provider credentials; not secret |
| AWS workload identity | how Kaana authenticates to Oxy; there is no service-key fallback ([Oxy identity](docs/operating.md#oxy-identity-temporary-infrastructure-workload)) |
| `KAANA_PUBLIC_API_ENABLED` | serves `https://kaana.ai/v1`; off, a merge is not an exposure |

Which providers are built in, how to declare another one, and the rollout gates for
a new provider are in [docs/provider-onboarding.md](docs/provider-onboarding.md) and
[docs/adapters.md](docs/adapters.md). A built-in adapter never invents a model list.

## Layout

```text
cmd/kaana/                              the serving process: both lanes, settlement worker, reloads
cmd/kaana-publisher/                    discovers models and re-issues the inventory on a cadence
cmd/kaana-credentials/                  one-shot admin: migrate and administer encrypted provider keys
cmd/kaana-credential-control/           signed customer-BYOK create/rotate/revoke task
cmd/kaana-platform-credential-control/  signed platform-pool import/rotate task
cmd/kaana-platform-credential-import/   operator-host client; signed HTTPS, secret on stdin
cmd/kaana-probe/                        liveness probe used by the canary
cmd/kaana-eval/                         latency benchmark through the public API
internal/publicapi/                     the public lane: grants, routes, preferences, settlement, catalogue
internal/httpapi/                       the signed lane
internal/kaana/                         the executor: routing, failover, retries, framing, usage
internal/realtime/                      realtime sessions, resume across tasks
internal/provider/                      the Adapter interface, key pools, error vocabulary, adapters
internal/inventory/                     which deployment serves which model, and staleness
internal/publisher/                     discovery, zero-retention checks, withholding, S3 output
internal/rotation/                      per-deployment breakers and latency / throughput readings
internal/credentialstore/               PostgreSQL: ciphertext, key state, outbox, cost events, KMS
internal/providercost/                  upstream provider cost and rate cards
internal/contract/                      Go types for the @kaana.ai/contracts wire
internal/edgeauth/                      Ed25519 verification of signed Oxy requests
packages/contracts/  packages/sdk/      the wire contract and the client
configs/                                public-catalogue.json, deployment-commercial.json, provider-rates.json,
                                        model-attribution.json, inventory.json (a reference snapshot for tests)
```

## Documentation

Start with [docs/lanes.md](docs/lanes.md), then the topic you are changing. Each
topic document ends with the rules a reviewer applies; `AGENTS.md` is the index.

| Topic | Documents |
|---|---|
| Shape of the system | [lanes](docs/lanes.md), [architecture](docs/architecture.md), [identity-and-routing](docs/identity-and-routing.md), [inference-rules](docs/inference-rules.md), [rules](docs/rules.md) |
| Routing and execution | [routing](docs/routing.md), [key-pools](docs/key-pools.md), [determinism](docs/determinism.md) |
| Inventory and publication | [inventory](docs/inventory.md), [operating](docs/operating.md) |
| Contract and API | [contract](docs/contract.md), [anthropic-messages](docs/anthropic-messages.md), [json-repair](docs/json-repair.md), [decisions](docs/decisions.md) |
| Providers | [adapters](docs/adapters.md), [provider-onboarding](docs/provider-onboarding.md), [openai-audio](docs/openai-audio.md), [elevenlabs](docs/elevenlabs.md), [deepgram](docs/deepgram.md), [realtime](docs/realtime.md) |
| Keys and money | [customer-provider-credentials](docs/customer-provider-credentials.md), [cost](docs/cost.md), [ADR 0010](docs/adr/0010-kaana-sells-and-invoices.md) |
| Decisions | [docs/adr/](docs/adr/) |
| Records of one-off rollouts | [schema-0013-cutover](docs/schema-0013-cutover-2026-09-24.md), [provider-credential-id-cutover](docs/provider-credential-id-cutover.md), [first-party-producer](docs/first-party-producer.md), [first-party-transport](docs/first-party-transport.md), [platform-activity](docs/platform-activity.md), [groq-token-capacity](docs/groq-token-capacity-2026-09-13.md), [scoped-attempt-claims](docs/scoped-attempt-claims.md) |

---

Tracked as workstream 13 of [OxyHQ/oxy#972](https://github.com/OxyHQ/oxy/issues/972).
