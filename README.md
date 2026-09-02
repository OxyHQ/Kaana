<p align="center">
  <img src="docs/assets/kaana-logo.svg" alt="Kaana" width="640">
</p>

# Kaana

**Kaana is Oxy's inference data plane.** One API executes requests across many
upstream providers and models — the same idea as OpenRouter, operated by Oxy for
Oxy and external customers. Its one canonical data-plane origin is
[`https://kaana.ai`](https://kaana.ai).

Apps do not all call Kaana in the same way. A one-shot product feature such as
translation or summarization goes through the Oxy inference edge to Kaana. A
conversation that needs memory, tools, approvals or an agent goes to **Alia**;
Alia performs that orchestration and invokes Kaana through the same Oxy edge.
Kaana never becomes the agent runtime merely because an agent eventually uses
a model.

A request names a **model**, not a vendor. Kaana decides which provider serves
it, translates the request for that provider's API, streams the answer back,
cancels it when the caller goes away, and reports what was consumed.

Oxy authorizes one ordered list of exact Kaana `deploymentId` values. It ranks
policy-qualified routes by explicit profile priority, then score descending,
using exact ID code units only to break an equal-score tie. Provider/model names,
insertion order and database order never select a route. Kaana verifies every
signed identity against one inventory snapshot and executes the list exactly as
received; it does not recompute the control-plane ranking.

## Checked-in reference inventory

`configs/inventory.json` is a measured reference snapshot, produced by reading
each provider's own authenticated catalogue rather than copying a third-party
catalogue. It is not a production-status page: the production publisher
discovers and re-issues its own snapshot, and that live object is the authority
for what Kaana serves now. `docs/inventory.md` explains the distinction.

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
already been authorized and reports what it measured. Accounts, Oxy login/API
credentials, provider-connection metadata, customer balances and the billing
ledger live in Oxy, which is the single control plane ([ADR 0005][adr0005],
[ADR 0006][adr0006]). Upstream provider keys are different: Kaana owns every one
as KMS ciphertext in its PostgreSQL credential store, including customer BYOK,
and never receives one through environment variables.

So the split is:

| Kaana (this repo) | Oxy |
|---|---|
| request normalization, provider adapters | accounts, organizations, projects |
| routing **execution**, failover, streaming, cancellation | authorization, scope checks, spend reservation |
| model deployments, provider health, circuit breakers | plans, balances, the billing ledger, the console |
| KMS-encrypted provider-secret custody | provider-connection metadata and policy |
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

Running it needs five things and refuses to start without them. There is no
unauthenticated mode, not even locally: a bypass that exists is a bypass that
ships.

| Variable | Meaning |
|---|---|
| `KAANA_INVENTORY_PATH` | the deployment inventory snapshot — see `configs/inventory.example.json` |
| `KAANA_EDGE_PUBLIC_KEYS` | `kid:base64,…` Ed25519 **public** keys the Oxy edge signs with; not secret |
| `KAANA_PROVIDERS` | the provider slugs this process serves, e.g. `cerebras,groq,xai,openrouter` |
| `DATABASE_URL` | TLS PostgreSQL connection for Kaana's credential database |
| `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN` | ARN of the symmetric KMS key that encrypts provider credentials; not secret |

Then per provider, `<SLUG>` upper-cased with `.` and `-` folded to `_`:

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDER_<SLUG>_PROTOCOL` | for an unknown slug | `openai_compatible` or `anthropic_messages` |
| `KAANA_PROVIDER_<SLUG>_BASE_URL` | for an unknown slug | the provider's API root |
| `KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT` | no | how long a spent or refused key stays out, default `15m` |
| `KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS` | no | `true` when the pool's keys are DIFFERENT accounts; only then does a throttle rotate |

Twenty-four providers carry a built-in protocol and API root: `openai`, `anthropic`,
`openrouter`, `cerebras`, `groq`, `xai`, `mistral`, `deepseek`, `sambanova`,
`siliconflow`, `ai21`, `google`, `together`, `cohere`, `fireworks`, `hyperbolic`,
`digitalocean`, `nvidia`, `modelscope`, `zai`, `nebius`, `nscale`, `chutes` and
`ovhcloud`. Alibaba remains explicit because its root is scoped by workspace
and region. Hugging Face and Kilo remain explicit because they are provider
routers, not direct inference providers. **Any other slug is servable by
declaring protocol and HTTPS root — no Go change.** Discovery and publication
remain separate gates; a built-in serving origin does not invent a model list.
Provider keys never enter this environment contract. They are ciphertext rows
in PostgreSQL, decrypted by the Kaana task through KMS and bound to their
`provider + keyId` identity with KMS encryption context.
The full contract, including what each failure mode looks like, is in
`docs/operating.md`.

## Layout

```
cmd/kaana/                      the binary: env config, wiring, graceful drain
cmd/kaana-publisher/            re-issues the inventory snapshot on a cadence
cmd/kaana-credentials/          migrate and administer encrypted provider keys
cmd/kaana-credential-control/   signed customer-BYOK create/rotate/revoke task
internal/contract/              Go types for @oxyhq/contracts' inference module
internal/credentialcontrol/     mutation and exact-outcome BYOK HTTP boundary
internal/credentialstore/       PostgreSQL ciphertext store and KMS boundary
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
configs/inventory.json          the measured snapshot, for a publisher to re-issue
configs/model-attribution.json  who RELEASED each model — the publisher's only editorial input
```

## Documentation

| | |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | the boundary with Oxy, the Oxy-facing surface, and what is deliberately out of scope |
| [`docs/identity-and-routing.md`](docs/identity-and-routing.md) | the Kaana/Alia boundary, canonical domain, exact deployment selection and product request paths |
| [`docs/routing.md`](docs/routing.md) | cancellation, same-model failover, circuit breakers and health scoring |
| [`docs/key-pools.md`](docs/key-pools.md) | several providers and a pool of keys for each; what a failure says about a KEY |
| [`docs/customer-provider-credentials.md`](docs/customer-provider-credentials.md) | customer BYOK custody, exact handles, task split and rollout contract |
| [`docs/inventory.md`](docs/inventory.md) | the deployment snapshot, how it is published, and what staleness costs |
| [`docs/adapters.md`](docs/adapters.md) | the adapter interface, the two ported adapters, the conformance harness |
| [`docs/provider-onboarding.md`](docs/provider-onboarding.md) | verified endpoints, catalog semantics and rollout gates for new providers |
| [`docs/operating.md`](docs/operating.md) | running it, and what a deployment must supply |
| [`docs/cost.md`](docs/cost.md) | upstream provider cost, and why it is not a customer amount |
| [`docs/contract.md`](docs/contract.md) | the wire contract, which is not authored here |
| `AGENTS.md` | the rules a reviewer applies |

---

Tracked as workstream 13 of [OxyHQ/oxy#972][epic].

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
