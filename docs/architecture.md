# Architecture and boundary

How Kaana splits between this data plane and Oxy's control plane, and what is deliberately not here. Read `../README.md` first.

## The boundary, in one paragraph

Kaana owns request normalization, provider adapters, routing **execution**,
streaming, cancellation, model deployments, provider health and technical
metering. Kaana never owns accounts, organizations, projects, members,
applications, credentials, balances, a billing ledger or a customer console. It
stores Oxy identifiers as **immutable, opaque strings** and never as records it
may create, edit or delete. Authorization, attribution, scope checks and spend
reservation all happen **in Oxy, before a request reaches Kaana** — Kaana does
not re-derive them, and an envelope that does not carry them is refused.

If a change would put an Oxy-owned concept in this repository, the change is
wrong, not the boundary. `AGENTS.md` states the rules a reviewer applies.


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
Kaana's own — the contract specifies shapes and says nothing about transport.

**Envelope versioning is a hard gate.** `schemaVersion` is read before anything
else is interpreted; an unrecognised version is refused whole. A version is
never inferred from the presence of a field. Conversely, **unknown fields are
tolerated**: the contract states that adding an optional field is additive, so a
strict decoder would turn every additive Oxy change into an outage here.

**Edge authentication is Ed25519 over the exact body** — Kaana holds only public
keys, so it cannot construct an envelope it would itself accept. This is a
decision Oxy has not made; the reasoning, and why it is not an HMAC, is in
`internal/edgeauth`. It is deliberately one small file to replace.


## Explicitly out of scope

Named here so nobody assumes otherwise. None of these is stubbed; each is simply
absent, and the code refuses rather than pretending.

- **Cross-model fallback.** Would require `routingFallbackPolicy`'s
  `authorizedCrossModel` list, which arrives only inside the policy snapshot
  Kaana is not sent. Nothing here can express it: the route-switch event Kaana
  builds is deployment-scoped by construction.
- **Failover without an operator acknowledgement.** Same-model failover is
  built, tested and off by default, for the contract reason above and in item 11
  below.
- **Reconciliation of provider cost against provider invoices.** Kaana measures
  what each request cost it upstream; matching that against what a provider
  actually billed is a finance process with no home in a data plane.
- **Oxy-hosted open-weight serving (vLLM/SGLang) and any GPU scheduler.** The
  epic says not to block the first API-only launch on a scheduler.
- **BYOK.** The provider-connection shapes are recorded not-applicable. The key
  pools are RELAY's own credentials with each provider; a customer's are an Oxy
  concept and stay one.
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
- **Modalities other than text.** Embeddings, images, audio and rerank are
  refused with `unsupported_modality` rather than mistranslated.
- **Routing-profile targets**, for the contract reason below.
- **Replay protection beyond the signature time window.** Kaana keeps no nonce
  cache; the edge owns request idempotency.


## What Oxy still has to decide

These surfaced while implementing against the contract, which has never been
implemented against before. Each is a real gap, not a preference.

1. **The envelope carries a routing policy *reference*, not a snapshot.**
   `inferenceRequestSchema.routingPolicy` is `{routingPolicyId, policyVersion}`.
   But ADR 0006 assigns *routing execution* to Kaana and ADR 0010 says the
   envelope carries "the resolved routing policy snapshot and its version". As
   published, Kaana **cannot** enforce provider allowlists, region residency,
   zero-data-retention, licence constraints or price ceilings — it has no
   values to enforce. Either the envelope must carry the snapshot, or the ADRs
   should say plainly that Oxy enforces all of it and Kaana only executes.
   Item 11 is the sharpest instance of this and the one that blocks working
   code.
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
   allocating it at step 1. Kaana implements the ADR's reading — Oxy allocates
   `requestId`, Kaana allocates `generationId` — and the comment should be
   corrected.
4. **No reservation or deadline in the envelope.** ADR 0010's `InferenceEnvelope
   v1` lists `reservation {reservationId, ceiling, priceVersion}` and `deadline`;
   `inferenceRequestSchema` has neither. So Kaana cannot enforce a spend ceiling
   or an execution deadline, and `normalizedUsageReportSchema` carries no
   `reservationId` (nor an `idempotencyKey`, though `usageReceiptSchema` has
   one) — Oxy must correlate settlement by `requestId` alone. Workable, but it
   should be a stated decision.
5. **`cached_input_tokens` and `reasoning_tokens` are not defined as subsets or
   siblings.** *Answered by OxyHQ/oxy#1019: the units **partition** a request,
   which is what Kaana already reported and what the ledger's arithmetic already
   assumed.* Two things follow that the answer does not cover, and the second
   adapter is where both surfaced.

   First, **the normalising subtraction is per provider and is not the same
   subtraction**. An OpenAI-compatible `prompt_tokens` includes its cached
   tokens; Anthropic's `input_tokens` is documented as excluding both of its
   cache counts, with the prompt total being
   `cache_read + cache_creation + input_tokens`. Copying the first adapter's
   arithmetic into the second would under-report every cached request. Both
   halves are now a conformance check rather than a comment.

   Second, **there is no unit for a cache WRITE**. Anthropic reports
   `cache_creation_input_tokens` separately and prices it at 1.25× to 2× the base
   input rate, against 0.1× for a read. Kaana folds writes into `input_tokens`,
   because the alternative — reporting them as `cached_input_tokens` — would
   price the most expensive input tokens in the request at the cheapest rate on
   the card. The units still partition the request; what is lost is the premium,
   on Kaana's own cost side. A `cache_write_input_tokens` unit would close it.
6. **The closed error set has no non-retryable platform-side failure.**
   *Answered by OxyHQ/oxy#1019, which added `provider_credential_invalid`
   (non-retryable), published in `@oxyhq/contracts@0.28.0` and adopted here.*
   Both adapters now report an upstream refusing the PLATFORM's credential under
   that code, with category `authentication`. The two halves pull in opposite
   directions on purpose: the code is non-retryable so a client stops hammering a
   request that cannot succeed, and the category is attributable so the breaker
   still takes the route out of rotation and a same-model failover to a
   deployment holding a DIFFERENT credential is still allowed. It is a
   conformance check, so the next adapter inherits it. Getting there also
   surfaced item 19, which is the larger of the two findings.

   **The neighbouring gap is closed too.** An upstream refusing to *bill* the
   platform is the same class of failure — only an operator can act, no retry
   helps — and reporting it as `quota_exceeded` was correct about retryability
   and wrong about whose account is exhausted, which reads as actionable while
   the action does nothing. `provider_billing_refused` landed in
   `@oxyhq/contracts@0.29.0`; Anthropic's 402 `billing_error` and an
   OpenAI-compatible `insufficient_quota` both map to it, and the conformance
   suite refuses any code that names the CUSTOMER's money for that scenario.
7. **Nothing specifies how Kaana authenticates the edge.** See
   `internal/edgeauth` for what Kaana implements and why it follows ADR 0012's
   asymmetric reasoning rather than a shared secret.
8. **The deployment descriptor has no upstream model identifier.**
   `modelDeploymentSchema` cannot express what a provider calls a model, so that
   mapping lives in Kaana's inventory. The same descriptor also carries
   `availabilityScope`, `commercialPermission` and `priceVersionId` — Oxy
   commercial decisions under ADR 0006 — so the shape currently has two owners
   and no stated direction of exchange.
9. **Nothing says who picks the current revision of an unpinned reference.** The
   contract says Oxy chooses it, but the envelope carries no resolution and the
   `start` event must report a revision-pinned reference — so in practice Kaana
   chooses. It does so from an explicit `current` flag in the inventory.
10. **Several produced shapes are not `.strict()`.** The stream events, the
    usage report and the error body all allow unknown keys, so a field Kaana
    emitted by mistake is silently stripped at Oxy's parse rather than caught.
    The request's `client` block *is* strict, and that strictness is what makes
    its privacy rule enforceable — the same argument applies to the rest.
11. **The customer's own switch for same-model failover never reaches the data
    plane that implements it.** `routingFallbackPolicySchema` carries
    `disabled`, `sameModelDeployment` and `authorizedCrossModel`;
    `routingPolicySchema` carries `allowedRegions` and `deniedRegions`. Every
    one of them governs what this repository's failover does, and the envelope
    carries none of them — only `{routingPolicyId, policyVersion}`. So a Kaana
    that failed over by default would override, for every customer who set it,
    a control the platform advertises to them. This build therefore ships
    failover **off**, and choosing among the deployments of one model at all is
    withheld with it, since that choice is governed by the same values. It
    cannot even be pre-implemented speculatively: adding a snapshot field to the
    Go request type fails the descriptor gate, because the published shape does
    not have one. **This is the concrete case for the snapshot travelling.**
    Failing that, Oxy should state that it resolves the deployment as well as
    the model, and send one — at which point Kaana's inventory and Oxy's
    catalogue need the direction of exchange that item 8 already asks for.
12. **The contract specifies event shapes and not their order.** Kaana emits
    `route_switch` *before* `start`, because the only switch it can safely
    perform is one where nothing has been streamed yet, and saying so in order
    is the truthful framing. The alternative reading — that `route_switch`
    amends a `start` already sent — is only expressible for a switch that
    happens mid-stream, which would duplicate output. If any consumer assumes
    `start` is always the first event, that assumption should become a stated
    rule in the contract rather than an implicit one.
13. **A model-scope `route_switch` cannot be constructed for a pinned request.**
    `routeSwitchDetail.requestedModelId` is the *unpinned* model line the
    customer asked for, so a request that pinned a revision has no value that
    satisfies the field. That is consistent with never substituting pinned
    weights, but it means cross-model fallback is expressible only for unpinned
    requests, which is worth stating rather than discovering.

The five below came from implementing the SECOND adapter. They are the ones a
single implementation could not have found, because each is a place where the
contract fits one provider's shape and not another's.

14. **`maxOutputTokens` is optional, and at least one provider requires it.**
    The Anthropic Messages API rejects a request with no `max_tokens`. The
    contract makes the field optional and says nothing about what its absence
    means, so an adapter has three options and two of them are wrong: invent a
    ceiling (silently truncates an answer the caller asked to be unbounded, and
    reports success), take one from the deployment descriptor (the same
    invention, moved to a config file nobody reads), or refuse. This build
    refuses, with `invalid_request` and `param: maxOutputTokens`, which means a
    request that is valid under the contract and served by one provider is
    refused by another. Either the contract should require the field, or
    `modelDeploymentSchema` should carry a per-deployment output ceiling and the
    contract should say that an absent value means it — but it should say which.

15. **There is no unit for cache-write tokens.** See item 5. A provider that
    prices a cache write at a premium and a cache read at a tenth of the input
    rate cannot be metered exactly against the published unit list.

16. **`refusal` had no finish reason of its own.** *Closed in
    `@oxyhq/contracts@0.29.0`.* The Messages API stops with
    `stop_reason: "refusal"` when the model declines; the contract's finish
    reasons ended at `content_filter`, so Kaana had to report a filter acting
    where the model had declined — different things to a customer deciding
    whether to rephrase, and a distinction the delta channels already carried.
    `refusal` is now a finish reason and this adapter emits it.

17. **No stream event can carry provider-opaque block metadata.** An
    extended-thinking response returns a `signature` per thinking block, and the
    provider REQUIRES those blocks back, unmodified, on the next turn of a
    tool-use conversation. `streamDeltaEvent` has `channel` and `text` and
    nothing that could hold it, and the request side has no content-part type
    for a thinking block either — so the round trip is not expressible in either
    direction, and multi-turn tool use with reasoning cannot be served through
    this contract at all. Kaana reads the signature so it cannot be mistaken for
    output, and drops it. This is a design decision rather than an oversight to
    patch: an opaque per-block blob crossing the boundary needs a home nobody has
    chosen yet.

18. **`safeErrorTextSchema`'s credential pattern was bearer-shaped, and
    redacting against it made a leak worse.** *Closed in
    `@oxyhq/contracts@0.29.0`.* The old pattern refused `authorization:`,
    `bearer <token>`, `api_key=` and `sk-…`. An upstream echoing
    `{x-api-key: <value>}` matched the **marker** and not the **value**, so
    redacting the match produced `{x-[redacted] <value>}` — which no longer
    tripped the refinement and was therefore *accepted* with the credential
    intact. The rewrite is four independent signals, one of which is a
    placeholder standing beside a surviving opaque value: the residue of exactly
    that span redaction.

    **Two things this repository has to keep doing, and they are the reason the
    item stays here rather than being deleted.** First, `SafeErrorText` no
    longer redacts a span — it withholds the whole message or none of it, since
    a span redaction is now both wrong and *refused*. Second, the published
    refinement is a last-resort refusal and says so: it cannot see a credential
    with no marker, no issued-token prefix and no placeholder beside it, because
    refusing those bytes means refusing request ids. `provider.RedactSecret`,
    applied by the adapter that still holds the bytes it sent, is the control —
    and it earns its place twice over. Where the pattern *does* recognise the
    shape, redacting the value is what keeps the customer's diagnostic instead
    of losing the whole message to the refusal; where it does not, redaction is
    the only thing between the credential and the customer.
    `internal/contract`'s fixture table pins both halves against the published
    schema itself, including a string that carries a secret and is accepted.

19. **A published version number did not identify the contract it names, and
    nothing gates that.** While this adapter was being written,
    `@oxyhq/contracts` on `main` and `@oxyhq/contracts@0.27.0` on npm had
    *different contents under the same version*: `main`'s `errors.ts` carried
    `provider_credential_invalid` and the published tarball did not, because
    #1019 merged after 0.27.0 shipped and did not bump the version. The
    immediate effect here was a code that could not be adopted; the lasting one
    is that "which 0.27.0 do you have" had no answer.

    **No in-repo consumer could have seen it.** Everything inside the monorepo
    resolves `workspace:*` and therefore reads `main`'s source; this repository
    is the first consumer that installs the published artefact, so the two
    copies had never been compared before. That is also why the fix belongs
    upstream rather than here.

    OxyHQ/oxy#1025 bumped and published `0.28.0`, closing the drift, and this
    build is generated against it. What is still open is the gate: nothing
    fails when a change to `packages/contracts/src` merges without a version
    bump, so the same divergence can reopen silently on any later PR. A CI check
    comparing the working tree's contract source against the published tarball
    for the version in `package.json` would answer it once.



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-relay-boundary.md
