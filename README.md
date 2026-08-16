# Relay

Relay is the inference **data plane** for the Oxy platform. It normalizes a
request, translates it for one upstream provider, streams the result back,
propagates cancellation, and reports what was technically consumed.

It is not a product. It has no customers, no accounts, no console and no
billing. Oxy is the single control plane for all of that
([ADR 0005][adr0005], [ADR 0006][adr0006]); Relay executes what Oxy has already
authorized and reports what it measured.

Tracked as workstream 13 of [OxyHQ/oxy#972][epic]. `Relay` is a working name
until naming review completes ([ADR 0011][adr0011]).

---

## The boundary, in one paragraph

Relay owns request normalization, provider adapters, routing **execution**,
streaming, cancellation, model deployments, provider health and technical
metering. Relay never owns accounts, organizations, projects, members,
applications, credentials, balances, a billing ledger or a customer console. It
stores Oxy identifiers as **immutable, opaque strings** and never as records it
may create, edit or delete. Authorization, attribution, scope checks and spend
reservation all happen **in Oxy, before a request reaches Relay** — Relay does
not re-derive them, and an envelope that does not carry them is refused.

If a change would put an Oxy-owned concept in this repository, the change is
wrong, not the boundary. `AGENTS.md` states the rules a reviewer applies.

## Layout

```
cmd/relay/                      the binary: env config, wiring, graceful drain
internal/contract/              Go types for @oxyhq/contracts' inference module
  descriptor.json               GENERATED from the published package
  descriptor_test.go            the drift gate
  drift_test.go                 the gate's positive control
  fixtures_test.go              wire fixtures for the Zod round-trip
internal/edgeauth/              Ed25519 verification of the Oxy edge's signature
internal/httpapi/               the Oxy-facing HTTP surface
internal/inventory/             deployment inventory: reference → provider + upstream model id
internal/provider/              the Adapter interface, registry and error vocabulary
  conformance/                  the suite every adapter must pass
  openaicompat/                 the ported OpenAI Chat Completions adapter
internal/relay/                 the executor: framing, terminality, usage reports
internal/sse/                   SSE decoding (upstream) and encoding (downstream)
tools/contract/                 Node tooling that derives and checks the contract
configs/inventory.example.json  an illustrative inventory
```

Layers depend downward only: `httpapi → relay → provider → contract`, with
`inventory` and `sse` as leaves. `contract` imports nothing of Relay's, which is
what lets the drift gate compare it against the published package with nothing
in between.

**Go** is the implementation language, as the epic prefers for a
high-concurrency streaming data plane. Nothing here argued against it: a
per-request goroutine with a cancellable `context.Context` threaded to the
upstream HTTP call *is* the cancellation design, rather than something layered
on top of it.

## The contract is not re-invented here

`@oxyhq/contracts@0.27.0` is the wire contract, and the Go types in
`internal/contract` are hand-written against it. Hand-writing is only safe
because two independent gates fail when the two sides diverge.

**1. A generated descriptor, compared field by field.**
`tools/contract/generate.mjs` imports every module under the published package's
`inference/` directory and emits `internal/contract/descriptor.json`: every
shape, field, optionality, kind, enum member, literal value and string
constraint. The shape list is **discovered, not listed**, so a shape added
upstream appears on the next regeneration.
`internal/contract/descriptor_test.go` then requires that:

- every published shape is either implemented in Go or recorded in an exact,
  reason-carrying `notApplicable` list (an exact count, so a shape cannot be
  excused by appending a line);
- every implemented shape's Go JSON field set **equals** the contract's — no
  extra field, no missing field;
- optionality matches (optional ⇔ absentable and `omitempty`, required ⇔ neither);
- enum members match exactly, in both directions;
- the regexes Relay enforces are character-identical to the published ones;
- `ContractVersion` equals the published `INFERENCE_CONTRACT_VERSION`.

CI regenerates the descriptor and fails on any diff, which catches a hand-edit
and a version bump nobody re-derived.

**What it catches:** a field renamed, added, removed, or flipped between
required and optional; a scalar's type changed; a reference repointed; a version
literal changed; a variant added to a discriminated union; an enum member added
or removed. `drift_test.go` proves this rather than asserting it — it perturbs
the descriptor in each of those eleven ways and requires the comparator to
report every one, after first confirming the unperturbed comparison is clean.

**2. Relay's own output, parsed by the real Zod schemas.**
Structure is not values. `go test ./internal/contract/...` marshals one fixture
per wire shape using the same Go types the server uses — with **every optional
field populated**, because an optional field that drifted is invisible in a
minimal fixture — and `tools/contract/validate.mjs` parses each with the
published schema itself. It also feeds six deliberately invalid fixtures that
the schemas must **reject**, and fails if it saw no fixtures at all: a validator
with a broken schema lookup would otherwise report the same clean run.

Regenerate after a version bump:

```bash
cd tools/contract && npm ci && npm run generate
```

## The Oxy-facing surface

```
POST /internal/v1/inference    signed envelope in, normalized event stream out
GET  /internal/v1/health       signed; the customer-safe provider projection
GET  /livez                    unsigned liveness; no provider or route detail
```

An HTTP status answers exactly one question: was this a well-formed envelope
from the Oxy edge? Once the answer is yes the response is `200` and an event
stream, and every outcome after that — including a refusal — arrives as the
stream's terminal `error` event. The alternative cannot be made to work: a
request that fails after two hundred tokens has already sent `200`.

Frames are named. `event: stream_event` carries a contract stream event;
`event: usage_report` carries the technical usage record. The framing is
Relay's own — the contract specifies shapes and says nothing about transport.

**Envelope versioning is a hard gate.** `schemaVersion` is read before anything
else is interpreted; an unrecognised version is refused whole. A version is
never inferred from the presence of a field. Conversely, **unknown fields are
tolerated**: the contract states that adding an optional field is additive, so a
strict decoder would turn every additive Oxy change into an outage here.

**Edge authentication is Ed25519 over the exact body** — Relay holds only public
keys, so it cannot construct an envelope it would itself accept. This is a
decision Oxy has not made; the reasoning, and why it is not an HMAC, is in
`internal/edgeauth`. It is deliberately one small file to replace.

## Cancellation

A client disconnect cancels the upstream provider call. The proof is split in
two, and each half is mutation-tested — the mutation was applied, the test was
observed to fail, and the file was restored:

| Link | Test | Mutation that must break it |
|---|---|---|
| client → executor | `internal/httpapi`: `TestClientDisconnectCancelsExecution` | `Execute(context.Background(), …)` instead of `r.Context()` |
| adapter → upstream | `internal/provider/conformance`: "a client disconnect cancels the upstream call" | `http.NewRequestWithContext(context.Background(), …)` in the adapter |

Both compare against a **control run that is not cancelled**, because "the
upstream saw its caller go away" is also what a request that simply finished
reports. The cancelled run must show the upstream observing the disconnect
*before* it wrote every chunk; the control must show it never observing one and
writing all of them.

A cancelled request still produces a usage report with the units measured up to
the cut, and settles as `cancelled`. A partial stream is a settlement case, so
an adapter that returned nothing on cancellation would make an exact refund
impossible.

## The provider adapter interface

```go
type Adapter interface {
	Provider() contract.ProviderSlug
	Translate(request *contract.Request, route Route) (*Call, error)
	Stream(ctx context.Context, call *Call, out Emitter) (Outcome, error)
	Health(ctx context.Context) Health
}
```

- **`Provider`** names the slug every event and usage record attributes work to.
  It comes from the adapter, not its registration site, so a mis-registration
  cannot mislabel a receipt.
- **`Translate`** is pure. A request the provider cannot express is refused
  before anything is spent upstream, and a pure translation is testable with no
  network — which is what makes covering that refusal cheap.
- **`Stream`** is execution, streaming, cancellation and usage measurement
  together, because they share one lifetime: the units are only correct if they
  come from the same read loop that saw the last frame before the cut.
- **`Health`** must be answerable without a customer request, so it cannot be
  folded into `Stream`.

What an adapter deliberately does **not** do: allocate ids, assign sequence
numbers, decide terminality, emit `done`/`error`/`route_switch`, resolve a model
reference to an upstream model id, or apply routing policy. Those are one
implementation in the executor, not one per provider. Adapters report semantic
content through `Emitter`, which stamps `requestId`, `sequence` and
`schemaVersion` itself — removing the whole class of bug where one provider's
events are unattributable or repeat a sequence.

## The ported adapter

`internal/provider/openaicompat` is a port of Alia's `openai` provider
(`packages/api/src/internal/providers/lib/providers/openai.ts`).

**Why that one.** Seven of Alia's adapters — openai, together, xai, cerebras,
hyperbolic, digitalocean, openrouter — are byte-identical apart from a base URL
and the word in their error string, because they all speak the OpenAI Chat
Completions protocol. Porting the *protocol* rather than a provider makes the
next six a `Config` and a conformance registration.
`TestOneProtocolServesSeveralProviders` runs the full suite under three more
slugs to keep that claim honest. Only `openai` is wired in `cmd/relay`: a
provider with no credential and no inventory entry would be a claim this build
cannot support.

**What the port deliberately changes.** Alia's `proxy()` returned the upstream's
raw stream to its caller — no normalization, no usage, no cancellation, no error
classification. It also never sent `stream_options.include_usage`, so **a
streamed request reported no usage at all**; a faithful port of that would be a
billing hole. And it substituted `temperature: 0.7` / `max_tokens: 8192` when the
caller set none, which silently changes every request nobody configured.

**No live provider call has been made from this repository.** There are no
provider credentials here, in the tests, or in CI. The adapter is exercised
against a fake upstream that speaks the real wire format, including its habit of
echoing the request's `Authorization` header back inside an error message.

## The conformance harness

`internal/provider/conformance` is the suite an adapter must pass. An author
supplies four things — how to build the adapter, how to start a fake upstream
speaking that provider's **real** wire format, one request the provider genuinely
cannot express, and the route it serves — and gets twelve checks back:

slug validity and stability · event framing (one `start` first, monotonic
sequences, `requestId` and `schemaVersion` on every event, exactly one terminal)
· a revision-pinned resolved model · the same normalized shape from a
non-streamed upstream · a provider that reports no usage settling as an estimate
· tool calls a client can reassemble · a transient 429 classified retryable ·
an exhausted quota on the same status classified non-retryable · **the
configured credential never reaching the customer**, with a control asserting
the upstream actually echoed it · a refusal that spends nothing upstream ·
cancellation, with its control · health with and without a credential.

The suite drives the adapter through the **real executor**, because an adapter is
only correct in the shape it is actually used.

## Running it

Everything comes from the environment and one inventory file. There is no
unauthenticated mode, not even for local development: a bypass that exists is a
bypass that ships.

| Variable | Required | Meaning |
|---|---|---|
| `RELAY_INVENTORY_PATH` | yes | deployment inventory (see `configs/inventory.example.json`) |
| `RELAY_EDGE_PUBLIC_KEYS` | yes | `kid:base64,…` Ed25519 **public** keys; not secret |
| `RELAY_PROVIDER_OPENAI_API_KEY` | no | absent ⇒ the provider reports `unconfigured` |
| `RELAY_PROVIDER_OPENAI_BASE_URL` | no | default `https://api.openai.com/v1` |
| `RELAY_ADDR` | no | default `:8080` |
| `RELAY_EDGE_MAX_SKEW` | no | default `5m` |
| `RELAY_MAX_ENVELOPE_BYTES` | no | default `16777216` |

```bash
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./...
cd tools/contract && npm ci && npm run generate && npm run validate
```

## Explicitly out of scope for this PR

Named here so nobody assumes otherwise. None of these is stubbed; each is simply
absent, and the code refuses rather than pretending.

- **Same-model deployment failover, circuit breakers and health scoring.** The
  inventory refuses two deployments of one revision rather than choosing
  silently; `routeSwitches` is structurally zero and no `route_switch` event can
  be emitted.
- **Cross-model fallback.** Would require the routing policy snapshot Relay is
  not sent.
- **Configuration snapshots for a control-plane outage.**
- **Provider-cost measurement and reconciliation.** Relay reports units, never
  money; there is no money type in `internal/contract` at all.
- **Oxy-hosted open-weight serving (vLLM/SGLang) and any GPU scheduler.** The
  epic says not to block the first API-only launch on a scheduler.
- **BYOK.** The provider-connection shapes are recorded not-applicable.
- **Modalities other than text.** Embeddings, images, audio and rerank are
  refused with `unsupported_modality` rather than mistranslated.
- **Routing-profile targets**, for the contract reason below.
- **Replay protection beyond the signature time window.** Relay keeps no nonce
  cache; the edge owns request idempotency.

## What Oxy still has to decide

These surfaced while implementing against the contract, which has never been
implemented against before. Each is a real gap, not a preference.

1. **The envelope carries a routing policy *reference*, not a snapshot.**
   `inferenceRequestSchema.routingPolicy` is `{routingPolicyId, policyVersion}`.
   But ADR 0006 assigns *routing execution* to Relay and ADR 0010 says the
   envelope carries "the resolved routing policy snapshot and its version". As
   published, Relay **cannot** enforce provider allowlists, region residency,
   zero-data-retention, licence constraints or price ceilings — it has no
   values to enforce. Either the envelope must carry the snapshot, or the ADRs
   should say plainly that Oxy enforces all of it and Relay only executes.
2. **A `routing_profile` target is unresolvable.** `routingTargetSchema` lets a
   request say "choose one for me", but the candidate list lives in the Oxy
   catalogue's `routingProfileSchema` and is not in the envelope. This build
   refuses such a request with `invalid_request` and `param:
   target.routingProfile` rather than picking a model, which would be exactly
   the silent substitution the platform forbids. Either Oxy resolves the profile
   before forwarding (in which case, why send the profile kind?), or the snapshot
   travels with the envelope.
3. **`requestId`'s owner is stated two different ways.** `identifiers.ts` says
   "a request id generated by the data plane", but it is *required* inside
   `attribution` on the inbound envelope, so it cannot be. ADR 0010 has the edge
   allocating it at step 1. Relay implements the ADR's reading — Oxy allocates
   `requestId`, Relay allocates `generationId` — and the comment should be
   corrected.
4. **No reservation or deadline in the envelope.** ADR 0010's `InferenceEnvelope
   v1` lists `reservation {reservationId, ceiling, priceVersion}` and `deadline`;
   `inferenceRequestSchema` has neither. So Relay cannot enforce a spend ceiling
   or an execution deadline, and `normalizedUsageReportSchema` carries no
   `reservationId` (nor an `idempotencyKey`, though `usageReceiptSchema` has
   one) — Oxy must correlate settlement by `requestId` alone. Workable, but it
   should be a stated decision.
5. **`cached_input_tokens` and `reasoning_tokens` are not defined as subsets or
   siblings.** Every OpenAI-compatible provider *nests* them: `prompt_tokens`
   includes cached, `completion_tokens` includes reasoning. The contract's units
   are a flat list. Relay reports them **disjoint** — input excludes cached,
   output excludes reasoning — so a price applied to every unit sums to the
   request. Under the other reading Oxy would double-charge cached and reasoning
   tokens. This is real money on a reasoning model and needs to be written down.
6. **The closed error set has no non-retryable platform-side failure.** Every
   customer-fault code is non-retryable and every platform/upstream code is
   retryable. So when an upstream refuses *Relay's own* credential, the honest
   classification (`provider_error`, category `authentication`) tells clients to
   retry a request that cannot succeed until an operator rotates a key. A
   non-retryable platform code — or making `service_unavailable` non-retryable —
   would close this.
7. **Nothing specifies how Relay authenticates the edge.** See
   `internal/edgeauth` for what Relay implements and why it follows ADR 0012's
   asymmetric reasoning rather than a shared secret.
8. **The deployment descriptor has no upstream model identifier.**
   `modelDeploymentSchema` cannot express what a provider calls a model, so that
   mapping lives in Relay's inventory. The same descriptor also carries
   `availabilityScope`, `commercialPermission` and `priceVersionId` — Oxy
   commercial decisions under ADR 0006 — so the shape currently has two owners
   and no stated direction of exchange.
9. **Nothing says who picks the current revision of an unpinned reference.** The
   contract says Oxy chooses it, but the envelope carries no resolution and the
   `start` event must report a revision-pinned reference — so in practice Relay
   chooses. It does so from an explicit `current` flag in the inventory.
10. **Several produced shapes are not `.strict()`.** The stream events, the
    usage report and the error body all allow unknown keys, so a field Relay
    emitted by mistake is silently stripped at Oxy's parse rather than caught.
    The request's `client` block *is* strict, and that strictness is what makes
    its privacy rule enforceable — the same argument applies to the rest.

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0006-oxy-relay-boundary.md
[adr0011]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0011-inference-data-plane-name.md
