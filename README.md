# Kaana

**Kaana is Oxy's own inference provider.** One API in front of many upstream
providers and many models — the same idea as OpenRouter, run by Oxy for Oxy.
Alia and every other Oxy app call it, and it is sold to customers outside Oxy
too. `kaana.ai`.

A request names a **model**, not a vendor. Kaana decides which provider serves
it, translates the request for that provider's API, streams the answer back,
cancels it when the caller goes away, and reports what was consumed.

## What it serves today

Measured on 2026-08-23 by reading each provider's own catalogue with that
account's key — not copied from documentation. `configs/inventory.json` is the
snapshot; `docs/inventory.md` explains how it is produced and re-issued.

| | |
|---|---|
| Providers | Cerebras, Groq, xAI, OpenRouter |
| Model deployments | 354 |
| Distinct models | 336 |
| Publishers represented | 53 |
| Models served by more than one provider | 8, one of them by three |

That last row is the point of the design. `openai/gpt-oss-120b` is one model
with three places to get it, and a request that names it is served or refused —
**never quietly answered by a different model.** See `docs/routing.md`.

## The two halves, and why this repository is only one of them

Kaana the product has accounts, plans, balances and a console. **None of that is
in this repository, on purpose.** This is the *data plane*: it executes what has
already been authorized and reports what it measured. Accounts, credentials,
customer balances and the billing ledger live in Oxy, which is the single
control plane ([ADR 0005][adr0005], [ADR 0006][adr0006]).

So the split is:

| Kaana (this repo) | Oxy |
|---|---|
| request normalization, provider adapters | accounts, organizations, projects |
| routing **execution**, failover, streaming, cancellation | authorization, scope checks, spend reservation |
| model deployments, provider health, circuit breakers | plans, balances, the billing ledger, the console |
| technical metering and upstream provider cost | what a customer is charged |

**Kaana measures units and never prices them.** There is no money type in
`internal/contract`, and adding one is the moment a second ledger starts. If a
change would put an Oxy-owned concept here, the change is wrong, not the
boundary — `AGENTS.md` has the rules a reviewer applies, and
`docs/architecture.md` the reasoning.

## Quick start

```bash
go build ./...
go test ./...
```

Running it needs three things and refuses to start without them. There is no
unauthenticated mode, not even locally: a bypass that exists is a bypass that
ships.

| Variable | Meaning |
|---|---|
| `KAANA_INVENTORY_PATH` | the deployment inventory snapshot — see `configs/inventory.example.json` |
| `KAANA_EDGE_PUBLIC_KEYS` | `kid:base64,…` Ed25519 **public** keys the Oxy edge signs with; not secret |
| `KAANA_PROVIDERS` | the provider slugs this process serves, e.g. `cerebras,groq,xai,openrouter` |

Then per provider, `<SLUG>` upper-cased with `.` and `-` folded to `_`:

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDER_<SLUG>_API_KEY` | no | one credential, or a comma-separated **pool**; absent ⇒ that provider reports `unconfigured` |
| `KAANA_PROVIDER_<SLUG>_PROTOCOL` | for an unknown slug | `openai_compatible` or `anthropic_messages` |
| `KAANA_PROVIDER_<SLUG>_BASE_URL` | for an unknown slug | the provider's API root |
| `KAANA_PROVIDER_<SLUG>_HEADERS` | no | `Name=Value` pairs, comma-separated — OpenRouter's attribution headers go here |
| `KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT` | no | how long a spent or refused key stays out, default `15m` |
| `KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS` | no | `true` when the pool's keys are DIFFERENT accounts; only then does a throttle rotate |

`openai`, `anthropic`, `openrouter` and `cerebras` carry a built-in protocol and
API root. **Any other slug is servable by declaring those two — no Go change.**
The full contract, including what each failure mode looks like, is in
`docs/operating.md`.

## Layout

```
cmd/kaana/                      the binary: env config, wiring, graceful drain
cmd/kaana-publisher/            re-issues the inventory snapshot on a cadence
internal/contract/              Go types for @oxyhq/contracts' inference module
internal/edgeauth/              Ed25519 verification of the Oxy edge's signature
internal/httpapi/               the Oxy-facing HTTP surface
internal/inventory/             which providers serve which model, and staleness
internal/kaana/                 the executor: routing, failover, framing, usage
internal/provider/              the Adapter interface, key pools, error vocabulary
  openaicompat/ anthropic/      the two ported adapters
  conformance/                  the suite every adapter must pass
internal/providercost/          upstream cost; never a customer amount
internal/rotation/              per-deployment circuit breakers and health scoring
internal/sse/                   SSE decoding (upstream) and encoding (downstream)
clients/typescript/             the published TypeScript client for this wire
tools/contract/                 derives descriptor.json and validates the wire fixtures
configs/inventory.json          the measured snapshot, for a publisher to re-issue
configs/model-attribution.json  who RELEASED each model — the publisher's only editorial input
```

`clients/typescript` is the one artefact here that is not the data plane: it is
the client Oxy callers use, published from this repository so the wire has one
implementation rather than one per app. It imports `@oxyhq/contracts` for the
shapes and declares only what this repository owns — the signing scheme, the SSE
frame names and the health projection. It carries no money type, and a test
derives that prohibition from the contract's own billing modules rather than
from a list.

## Documentation

| | |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | the boundary with Oxy, the Oxy-facing surface, and what is deliberately out of scope |
| [`docs/routing.md`](docs/routing.md) | cancellation, same-model failover, circuit breakers and health scoring |
| [`docs/key-pools.md`](docs/key-pools.md) | several providers and a pool of keys for each; what a failure says about a KEY |
| [`docs/inventory.md`](docs/inventory.md) | the deployment snapshot, how it is published, and what staleness costs |
| [`docs/adapters.md`](docs/adapters.md) | the adapter interface, the two ported adapters, the conformance harness |
| [`docs/operating.md`](docs/operating.md) | running it, and what a deployment must supply |
| [`docs/cost.md`](docs/cost.md) | upstream provider cost, and why it is not a customer amount |
| [`docs/contract.md`](docs/contract.md) | the wire contract, which is not authored here |
| `AGENTS.md` | the rules a reviewer applies |

## It was called Relay

`Relay` was the working name, and the git history passes through `Pensara` for
one commit — chosen and replaced before anything shipped under it. Kaana is the
name.

The repository directory, the AWS resources, the `RELAY_*` variables a
deployment sets and the `X-Oxy-Relay-*` headers Oxy's edge signs still carry the
old name, and two of those are answered under **both** spellings while the
services that set them are migrated:

- **Environment.** Everything is `KAANA_*`; a deployment still setting `RELAY_*`
  is answered from that, current name winning. One function,
  `providerconfig.EnvName`.
- **The edge's signing headers.** `X-Oxy-Kaana-*` is preferred and
  `X-Oxy-Relay-*` still verifies. A clean cut would refuse every request with
  `unknown key id` — before a signature is even computed — for as long as the
  edge kept sending the old names.

Both are migrations with an end, not compatibility layers: deleting either turns
a named test red, which is how the deletion proves it did something.

Three things say `relay` deliberately and are not oversights — the signed domain
separator `oxy-relay-envelope:v1`, which lives inside the signature; the shipped
binary paths and the `/etc/relay` mount point, which the task definition names;
and the image's own `ENV` defaults, because a container definition's environment
beats an image `ENV` while the binary prefers the current name, so swapping them
early would invert the override.

---

Tracked as workstream 13 of [OxyHQ/oxy#972][epic].

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-relay-boundary.md
