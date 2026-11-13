# Architecture and boundary

How Kaana splits between this data plane and Oxy's control plane, and what is deliberately not here. Read `../README.md` first; for the caller-facing concepts (exact model, power level, app default) read the [Oxy inference developer guide](https://github.com/OxyHQ/oxy/blob/main/docs/inference/README.md).

## The boundary, in one paragraph

Kaana owns inference normalization, catalogue and power levels, routing policy,
provider execution, metering and, under the accepted
[ADR 0010](adr/0010-kaana-sells-and-invoices.md), economic admission, accounting
and invoicing. Oxy owns accounts, organizations, applications and credentials;
Kaana treats their identifiers as opaque identity facts. Gemies owns product
conversations, agents, memory, tools and approvals.

The financial migration is staged. Existing Oxy-signed requests and grants
remain authoritative until their population's verified cutover. Kaana-native
admission requires explicit enrollment and a durable policy/idempotency record;
it cannot be enabled by interpreting an identity assertion as a spend grant.
Provider credentials remain exclusively KMS ciphertext in Kaana's PostgreSQL
store. Customer money and upstream provider cost are separate accounting
boundaries, not reasons to copy Oxy's identity entities.


## The surfaces

Kaana has two ways in, the public lane (`https://kaana.ai/v1`) and the signed lane
(`/internal/v1/*`). Who decides what on each is in [lanes.md](lanes.md); this
section describes the signed lane's HTTP surface.

Provider keys, what providers charge for them and which key serves are
Kaana's ([ADR 0001](adr/0001-kaana-owns-provider-keys-costs-and-routing.md)):
exact opaque key identity, stated funding (free, credits, paid), upstream
verdict, evidence source, observation time and recovery lease. Kaana walks a
deployment's key pool cheapest first and never beyond its admission's funding
ceiling. Oxy retains identity and API-key authority. Kaana owns inference
policy and customer economics; legacy Oxy-owned admissions keep their original
financial authority until the staged migration in ADR 0010 completes. Kaana does not infer funding from a
credential or turn its technical state table into a wallet.

```
POST /internal/v1/inference    signed envelope in, normalized event stream out
GET  /internal/v1/realtime     WebSocket; the signed first frame opens or resumes a session
GET  /internal/v1/health       signed; the customer-safe provider projection
GET  /livez                    unsigned liveness; no provider or route detail
```

There is no metrics route. Operational metrics are CloudWatch EMF lines on stdout
([operating](operating.md#metrics)), so nothing has to scrape the data plane.

An HTTP status answers exactly one question: was this a well-formed envelope
from the Oxy edge? Once the answer is yes the response is `200` and an event
stream, and every outcome after that — including a refusal — arrives as the
stream's terminal `error` event. The alternative cannot be made to work: a
request that fails after two hundred tokens has already sent `200`.

Frames are named. `event: stream_event` carries a contract stream event;
`event: usage_report` carries the technical usage record. The framing is
Kaana's own — the contract specifies shapes and says nothing about transport.

**Envelope versioning is a hard gate.** `schemaVersion` is read before anything
else is interpreted; an unrecognised version is refused whole. A version is
never inferred from the presence of a field. Conversely, **unknown fields are
tolerated**: the contract states that adding an optional field is additive, so a
strict decoder would turn every additive Oxy change into an outage here.

**Edge authentication is Ed25519 over the exact body** — Kaana holds only public
keys, so it cannot construct an envelope it would itself accept. Oxy adopted
the scheme as its ADR 0015 (accepted 2026-08-17); the reasoning, and why it is
not an HMAC, is in `internal/edgeauth`.


## Explicitly out of scope

Named here so nobody assumes otherwise. None of these is stubbed; each is simply
absent, and the code refuses rather than pretending.

- **Reconciliation of provider cost against provider invoices.** Kaana measures
  what each request cost it upstream; matching that against what a provider
  actually billed is a finance process with no home in a data plane.
- **Oxy-hosted open-weight serving (vLLM/SGLang) and any GPU scheduler.** The
  epic says not to block the first API-only launch on a scheduler.
- **BYOK policy and metadata.** Oxy owns provider-connection status, scope,
  validation and customer authorization. Kaana owns encrypted secret custody
  under an opaque handle and can execute only the exact credential generation
  signed onto an authorized route. It never selects a connection, grants BYOK
  from provider/account metadata or falls back to a platform credential; the
  customer lane also neither reads nor updates the platform credential lane's
  deployment breaker. The custody, resolver and production gates are specified in
  `customer-provider-credentials.md`.
- **An official quota API or authenticated usage endpoint.** It would sit above
  the response-header mapping in the preference order and is not implemented:
  every provider spells one differently, none can be exercised from a repository
  with no credentials and no live provider call, and a poller written against
  documentation alone is the same guess in a more expensive form. Exhaustion is
  learned from the provider's own refusal instead, which is a stronger signal
  and arrives on the request that would have been refused anyway.
- **A threshold-based `approaching_limit` quota state.** It would need a
  threshold nobody has chosen, and nothing in this build would act on it: a key
  is usable or it is not.
- **Modalities other than text, embeddings, speech, transcription, image
  generation and rerank.** Embeddings are served only where the capability table
  (`providerconfig.ExecutableAPIFormats`) lists an embeddings endpoint: today
  the OpenAI-compatible adapter's `POST {base}/embeddings` for `siliconflow`
  and `openrouter`, non-streaming, text or text-batch input, at a fixed 1024
  dimensions. Speech and transcription have their own endpoints
  (`POST /v1/audio/speech`, `POST /v1/audio/transcriptions`; see
  [openai-audio](openai-audio.md)) and image generation is
  `POST /v1/images/generations`, served by the OpenAI-compatible adapter where the
  capability table lists it (see [adapters](adapters.md#image-generation-contract-set-8120)). Rerank is `POST /v1/rerank`, served
  by the OpenAI-compatible adapter where the capability table lists it
  (`openrouter` only; see [adapters](adapters.md#rerank-contract-set-8150)) and refused with
  `unsupported_modality` for a model no deployment can serve, which until the
  operator declares a rerank model in the catalogue is every model. Any other
  modality is refused with `unsupported_modality` rather than mistranslated.
- **A batch API** (asynchronous job submission, a 24-hour window, polling for
  results). Kaana is synchronous and stateless, may not store a prompt or a
  completion, and Oxy's ledger cannot hold a reservation that long; `batches`
  stays in `APIFormat` as a reserved name. See
  [ADR 0011](adr/0011-batch-api-out-of-scope.md).
- **Replay protection beyond the signature time window.** Kaana keeps no nonce
  cache; the edge owns request idempotency.
- **Realtime translation sessions and customer (BYOK) credentials for a
  session.** Conversation sessions (with input transcription) and, under
  `xai-stt`, transcription sessions (ADR 0006) are served on platform keys;
  each of the rest is refused by name (`realtime.md`, "Findings, refusals and
  what is not verified").


## Contract findings

The log of what surfaced while implementing against the contract, with what each
finding settled, is [architecture-findings.md](architecture-findings.md). It is
history, not a rule; the rules are below.

## Rules a reviewer applies

The ownership split above and ADR 0010 govern new work. Preserve original
Oxy authority for legacy requests while Kaana-native admission is staged.

- **An Oxy ID is an immutable opaque string.** Do not recreate Oxy identity
  entities, parse their IDs or use them as local primary keys. Kaana-owned
  financial records use local keys and carry authenticated Oxy references.
- **Identity is not spend authority.** Native policy is reviewed and bound to
  the exact verified principal. Public application attribution and session
  identity cannot impersonate a workload or choose an application's payer.
  Re-read identity through its owner; do not replicate credentials or invent
  scopes.
- **One admission has one financial authority.** Preserve signed legacy
  constraints, immutable native policy/price snapshots and the cross-cutover
  idempotency fence. Timeout and retry cannot change ledgers or execute twice.
- **Separate customer money from technical usage and upstream cost.** Usage
  reports remain units; provider costs are private operator accounting. Only
  the canonical financial/receipt contract may expose customer amounts.
- **Source is not rollout.** No native enrollment, schema application, balance
  transfer or invoice delivery is established by this architecture document.
