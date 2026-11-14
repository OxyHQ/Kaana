# Contract set history

> **Historical record**, newest first, moved out of [contract.md](contract.md) so that document
> holds the rules. The release notes of each published `@kaana.ai/contracts` version, 8.0.0 on, are
> in [`packages/contracts/CHANGELOG.md`](../packages/contracts/CHANGELOG.md); this file also carries
> sets 3.8.0 to 7.0.0, which that changelog never recorded, and the longer reasoning behind each set.

## Contract set 8.17.0: the catalogue in the reader's set

MINOR, `@kaana.ai/contracts` 8.17.0. No shape, field or member changes; every
8.16.0 message parses the same. The catalogue's leaves are strict and its enums
closed, so a reader refuses the WHOLE `GET /v1/models` list on the first value its
set lacks: 8.16.0's `live` and `defaultKinds` would have refused every model to
`@kaana.ai/sdk` 3.5.0 (contracts 8.2.0, Gemies and Mention), and the
`cache_write_input_tokens` unit price (8.4.0), already declared on six
platform-internal Anthropic lines, refuses it wherever such a line is listed with
the declared price.

`CATALOGUE_ADDITIONS` lists each value the catalogue gained after a set, with the
set that introduced it: `capabilities.unreported` and the unit
`cache_write_input_tokens` (8.4.0), the session kind `live` and
`capabilities.realtime.defaultKinds` (8.16.0). Kaana renders the catalogue in the
set the request states in `Kaana-Contract-Version`, which `@kaana.ai/sdk` has sent
on every request since 2.0.0, leaving out every value that set lacks: a list
element holding a withheld member, and the field the entry names when that
empties a required list (a line holding only `live` lists no
`capabilities.realtime` for an 8.2.0 reader). A request that states no set, or
one that does not parse, receives none of them. The fix is on the server, so it
covers every deployed reader with no client bump; a value reaches a reader once
its SDK states a set that has it. `capabilities.unreported`, until now withheld
from every public reader, is served to readers on 8.4.0 or later. MINOR because
Kaana now answers by the stated set, not because any byte parses differently.

The `@kaana.ai/sdk` compatibility suite holds `CATALOGUE_ADDITIONS` to the
difference between `modelSchema` and each pinned release's (now also sdk 3.3.0,
3.5.0, 3.16.0 and contracts 8.0.1, 8.2.0, 8.15.0, 8.16.0), and each release lists
the catalogue as Kaana renders it for the set it states. The signed
`GET /internal/v1/models` declared document is unchanged: Oxy parses it with its
own pinned contracts and adopts a set before a declared value from it.

## Contract set 8.16.0: live sessions with delegation (ADR 0012)

MINOR, `@kaana.ai/contracts` 8.16.0. Additive; everything 8.15.0 parses still
parses. The realtime session kind `live` is a spoken conversation whose turns the
caller's backend answers, so a call has the tools, memory and model of a typed
turn and changing the voice provider is a catalogue change. Its
`config.delegation` (`acknowledgement: auto | none`) is required for `live` and
refused elsewhere; the commands `delegation.reply`, `delegation.context` and
`delegation.cancel` answer the events `delegation.requested` and
`delegation.superseded`. `capabilities.realtime.defaultKinds` names the line a
client opens for a kind, at most one active line per kind. MINOR rather than
PATCH because a closed enum and two discriminated unions gain members: a consumer
on 8.15.0 refuses a `live` session, its commands and events and a catalogue
naming a default. No line lists `live` until its adapters (ADR 0012 phases K2,
K3) pass `TestLiveContract`.

## Contract set 8.14.0: verified product session identity

MINOR, `@kaana.ai/contracts` 8.14.0. Adds a distinct `session` identity
variant on the `product_session` lane. `verifiedRequester` separates the person's
subject account and session user from `applicationOwnerAccountId`.
`applicationBinding` states whether the app came from a session binding, platform
proof, or public client id; a public client id is attribution, not app authority.
The assertion's `userId` is the verified subject account. Requested attribution
must be absent or equal to that subject. No payer, scopes, spending permission,
price, route or balance is inferred from a session. Existing workload and external
credential wire shapes remain unchanged. The Go identity model represents the
variant-specific fields explicitly; economic authority types are unchanged.

## Contract set 8.13.0: identity assertions without economic authority

MINOR, `@kaana.ai/contracts` 8.13.0. Adds `kaanaIdentityRequestSchema`,
`kaanaIdentityPrincipalSchema` and `kaanaIdentityAssertionSchema` to the internal
entry. Oxy asserts live workload or external-key identity under the separate
`oxy-kaana-identity:v1` signature domain; assertions bind the exact authentication
request and expire within 60 seconds. They contain no inference content, model,
route, spending policy or reservation. User attribution is not person authority.
Product sessions are not admitted by this identity contract.

Existing grants, settlements and public request/response shapes are unchanged.
This foundation does not activate native Kaana admissions or migrate balances;
those require their own implementation and reviewed enrollment.

## Contract set 8.15.0: the rerank request in the envelope

MINOR, `@kaana.ai/contracts` 8.15.0. Additive; everything 8.14.0 parses still
parses. The signed envelope gains the optional `rerank` member
(`inferenceRerankParametersSchema`: `documents`, `topN`, `returnDocuments`) so
`POST /v1/rerank` can be executed: the query is the envelope's text input, and
`Input` has no member for a list a query is ranked against, so without it an
adapter would receive a query and none of its documents. Required with, and valid only
with, the `rerank` API format, text modality, a non-empty text query and no streaming.
The public rerank shapes and the grant are unchanged.

## Contract set 8.12.0: image generation controls in the envelope

MINOR, `@kaana.ai/contracts` 8.12.0. Additive; everything 8.11.0 parses still
parses. The signed envelope gains the optional `image` member
(`inferenceImageParametersSchema`: `n`, `size`, `quality`, `style`,
`responseFormat`) so `POST /v1/images/generations` can be executed: without it an
adapter would receive a prompt and none of the caller's controls. Valid only with
`images_generations`, image modality and text input. The public request and
response shapes are unchanged.

## Contract set 8.11.0: the rerank operation (contract only)

MINOR, `@kaana.ai/contracts` 8.11.0. Additive; everything 8.10.0 parses still
parses. `grantOperationSchema` gains `rerank` (appended last: Oxy asserts the list
member for member) and `rerankRequestSchema` / `rerankResponseSchema` describe
`POST /v1/rerank`. Nothing in Kaana emits the operation or serves the route; Oxy
supports it first. No usage unit is added: `requests` and `input_tokens` carry
it, and the hold is bounded by the grant request's `estimatedInputTokens`.

## Contract set 8.10.0: price controls in `provider` (ADR 0008)

MINOR, `@kaana.ai/contracts` 8.10.0. Additive; everything 8.9.0 parses still
parses. `provider.max_price` (`prompt` and `completion`, USD per million tokens)
and `provider.sort: "price"` read the declared list price of a model line, the
price Oxy rates the grant at. They decide which line serves the request, never
expose an amount, and the signed grant and envelope are unchanged.

## Contract set 8.9.0: opt-in JSON repair (ADR 0009, decision 5)

MINOR, `@kaana.ai/contracts` 8.9.0. Additive; everything 8.8.0 parses still
parses. `POST /v1/responses`' `responseFormat` gains `repair?: boolean` on
`json_object` and `json_schema` (`responsesResponseFormatSchema`), and a repaired
answer carries the response header `Kaana-Repaired: true` (`KAANA_REPAIRED_HEADER`),
never a body member, so every reader of an older contract parses what Kaana
serves. The signed grant and envelope are unchanged: `repair` is a public request
member that never reaches Oxy or a provider, it is not a request-parameter word
and it does not narrow route selection. The rules, the order of repairs and what
is never touched are in [json-repair.md](json-repair.md).

## Contract set 8.8.0: prompt-cache breakpoint

MINOR, `@kaana.ai/contracts` 8.8.0. Additive: an optional boolean
`cacheBreakpoint` on a `text`, `image` or `file` content part (so on a system or
developer message's parts) and on a tool definition (not a realtime tool).
Everything 8.7.0 parses still parses. It asks a provider that can express it to
cache the prompt up to and including that part, system message or tool, and means
Anthropic's `cache_control: {type: "ephemeral"}` with the default 5-minute
lifetime and nothing else: there is no lifetime member because the one
cache-write unit prices only that write. It is a hint:

- a route that cannot express it ignores it and still succeeds (every adapter
  but Anthropic's);
- the Anthropic adapter sends at most four (`contract.MaxCacheBreakpoints`), the
  earliest in prompt order: tools, system and developer messages (hoisted, so
  ahead of the conversation), then the conversation; a system, developer or tool
  message is one block and takes one slot however many parts are marked
  (`contract.LimitCacheBreakpoints`);
- the executor sends it only to a route whose granted price version prices
  `cache_write_input_tokens` (`kaana.withheldCacheBreakpoints`), per route, so an
  unpriced fallback never provokes a write it could not settle, whatever endpoint
  carried the marker. `/v1/messages` decides the same before it answers, so its
  `Kaana-Ignored-Parameters` header is exact
  ([anthropic-messages.md](anthropic-messages.md#cache_control-honoured-where-it-can-be-priced-ignored-elsewhere)).

The member is on the shared part and tool shapes, so `/v1/chat/completions` and
`/v1/responses` also accept it under the same rules; they carry no header for a
marker withheld from an unpriced route.

## Contract set 8.6.0: reasoning continuity (ADR 0009, decision 2)

MINOR, `@kaana.ai/contracts` 8.6.0. Additive; everything 8.5.0 parses still
parses. The owner reverted the earlier decision to leave reasoning out of
`messages.ts` ("an opaque per-block blob needs a home nobody has chosen yet") and
chose the home below; the reason is correctness: a thinking model that calls a
tool cannot be continued unless its reasoning blocks come back unmodified and in
order (Anthropic requires its thinking blocks and signatures, OpenRouter an
ordered `reasoning_details` array).

- **`reasoning: {effort?, maxTokens?, exclude?}`** (strict). `effort` and
  `maxTokens` (a thinking budget in tokens) are alternatives, refused together.
  `exclude: true` means the model may still reason, and be billed for it, but no
  reasoning is returned (no delta, no part, no token); Kaana honours it itself,
  on every route, so it is not a route parameter. `{}` is valid and opts in.
  `reasoning.maxTokens` is a new request-parameter word (`provider.ParameterReasoningMaxTokens`;
  OpenRouter's `reasoning` states it); an adapter that cannot take a budget
  refuses it with `invalid_request` naming `reasoning.maxTokens`.
- **The `reasoning` content part**, `{type: 'reasoning', text?, opaque?}`, an
  assistant message only (refused elsewhere like `refusal`), at least one of the
  two members. `text` is for people and is never forwarded alone. `opaque` is a
  sealed continuation token the customer returns UNMODIFIED in the position it
  arrived. Its content is internal (`internal/reasoningblock`: base64url JSON
  `{f: <family>, p: <payload>}`); the public name and shape name no provider.
  Sealing is an encoding, not a signature: a forged or edited token only gets the
  customer the provider's own 400, and nothing relies on it for trust.
- **The `reasoning` delta** may carry `opaque`, once per finished block after its
  text deltas, with empty `text`. It is emitted only to a request that carried
  `reasoning`, and a whole answer (`/v1/responses`, `/v1/messages`) carries
  reasoning parts only for such a request, because a reader of an older contract
  refuses a part type it does not know. The compatibility gate
  (`compatibility.test.ts`) parses a delta with `opaque` under contracts 4.4.0 and
  5.0.0. Oxy parses messages and the delta on the legacy signed lane, so it only
  needs to bump its pin for that lane; the public/grant lane never sends messages
  to Oxy.
- **Which provider reads which block.** An adapter forwards only blocks of its
  own family and drops every other, in order, never failing the request: after a
  failover or a model switch the continuation is lost, the conversation is not.
  Anthropic sends `thinking` / `redacted_thinking` ahead of the text and
  `tool_use` they preceded; OpenRouter's adapter sends the `reasoning_details`
  entries unmodified in order; every other route receives none.
- **Surfaces.** `/v1/responses` carries the part and the delta token.
  `/v1/messages` maps `thinking: {type: enabled, budget_tokens}` to
  `reasoning.maxTokens`, `thinking: {type: adaptive}` to `reasoning: {}`, and
  returns `thinking` blocks whose `signature` is the token (see
  [anthropic-messages.md](anthropic-messages.md)). **`/v1/chat/completions`
  carries none**: OpenAI's dialect has no standard field for it and a
  non-standard one would break the clients that parse it, so a reasoning part
  there is refused naming this decision and no chat answer or chunk ever carries
  reasoning. Reasoning continuity is on `/v1/responses` and `/v1/messages`.
- Reasoning blocks never enter the usage estimate beyond what a provider
  reports, and are never logged or persisted (customer data).
## Contract set 8.5.0: provider preferences and prompt-cache key (ADR 0008)

MINOR, `@kaana.ai/contracts` 8.5.0. Additive; everything 8.4.0 parses still
parses. `providerPreferencesSchema` is the optional `provider` object on chat
completions and responses (`order`, `only`, `ignore`, `allow_fallbacks`,
`require_parameters`, `quantizations`, `sort`, `preferred_max_latency`,
`preferred_min_throughput`), plus `prompt_cache_key` / `promptCacheKey`. It
narrows what Oxy's grant admitted and never widens it; the signed grant and
envelope are unchanged. A request names providers, a response never does.

## Contract set 8.4.0: cache-write unit and unreported capabilities

MINOR, `@kaana.ai/contracts` 8.4.0. Additive and declared only: nothing emitted
either when it shipped, and `configs/public-catalogue.json` was unchanged. A
consumer on 8.3.0 refuses what is new, so Oxy adopts it first. Oxy has (OxyHQ/oxy#2253);
the unit is now emitted where a grant prices it (see [cost.md](cost.md#cache-write-tokens)),
with no contract change.

- Usage unit `cache_write_input_tokens`: input tokens written to a prompt cache
  on this request (the premium on the first request). A sibling under the
  partition rule; one unit, the provider's default TTL only (a longer TTL is not
  supported). See [architecture](architecture.md) finding 5.
- `capabilities.unreported`: the boolean capabilities whose `false` means "not
  recorded", not "known absent". The five booleans stay required because
  deployed readers parse them; a key listed here must be `false`; absent means
  undeclared, like `apiFormats`. `modelCapabilitiesSchema` is `notApplicable` in
  the Go descriptor test, so no Go struct changes. The declared catalogue now
  populates it ([operating](operating.md#the-declared-catalogue)); the five
  booleans themselves did not change.

## Contract set 8.3.0: Anthropic Messages (`POST /v1/messages`)

MINOR, `@kaana.ai/contracts` 8.3.0. Additive; everything 8.2.0 parses still
parses. `anthropicMessagesRequestSchema`, `anthropicMessageSchema`,
`anthropicStreamEventSchema` and `anthropicErrorSchema` are public shapes only:
the endpoint is served as the `responses` operation, so the signed grant and
envelope are unchanged. All four are `notApplicable` to the Go descriptor test
(Go decodes the request by hand and the wire-fixture gate parses every body it
writes with these schemas). What is supported and what is refused, and why
`cache_control` is accepted and forwarded or ignored, is in [anthropic-messages.md](anthropic-messages.md).

## Contract set 8.2.0: image input (ADR 0007)

MINOR, `@kaana.ai/contracts` 8.2.0. Additive; everything 8.1.0 parses still
parses, and a consumer on 8.1.0 or earlier refuses what is new, so Oxy adopts
it first.

- **`checkImageInput(messages)`** (public): a conversation's image count and
  every issue against the image rules. Inline base64 only (a `url` source is
  refused: nothing fetches an image URL or forwards one), one of
  `INPUT_IMAGE_MEDIA_TYPES` (PNG, JPEG, WebP, GIF), at most
  `MAX_INPUT_IMAGE_BYTES` (3,750,000) decoded bytes, on a `user` or `tool`
  message, at most `MAX_INPUT_IMAGES` (20) a request. An issue names the
  message, the part and the field, never the image. `base64DecodedLength`
  counts without decoding.
- **`declaredModelSchema.imageInput`** (`/internal`, `declaredImageInputSchema`):
  `{ maxInputTokensPerImage }`, the most input tokens one admitted image costs
  on the line at any `detail`, from the publisher's documented rule. Requires
  `image` in `inputModalities`. Absent: no request carrying an image is
  authorized on the line. Not served on `GET /v1/models`. Relayed as raw JSON
  with the declared catalogue (`notApplicable` to the Go descriptor test).
- **`grantRequestFactsSchema.inputImages`**: the count of image parts, 1 to 20,
  present exactly when a completion carries one; image bytes are not in
  `estimatedInputTokens`. Go: `GrantRequestFacts.InputImages`.

## Contract set 8.1.0: interim text of a transcription session (ADR 0006)

MINOR, `@kaana.ai/contracts` 8.1.0. Additive; everything 8.0.x parses still parses.

- **`inputAudioTranscription.interimResults`** (optional boolean): a
  `transcription` session asks for interim text. `realtimeConfigIssues`
  refuses `true` on any other kind, at `config.inputAudioTranscription.interimResults`;
  Go: `RealtimeInputTranscription.InterimResults`, refused by
  `ValidateSession` the same way.
- **`transcript.interim`** `{source: 'input_audio', itemId, contentIndex, text}`
  joins both server-event unions: the provisional text of an item not yet
  final. Each replaces the item's previous one; it is never settled, never part
  of the transcript, and superseded by the item's next `transcript.delta` or its
  one `transcript.done`. Go: `RealtimeTranscriptInterimEvent`.
- The signed-envelope lane cannot open a transcription session at all (it
  refuses input transcription, `kaana.EnvelopeInputTranscriptionRefused`), so
  it never sees the new event.

## Contract set 4.4.0: deployment commercial facts and `sameModelFailover` (ADR 0002, amendment 2026-10-15)

MINOR, `@kaana.ai/contracts` 4.4.0. Additive; everything 4.3.0 parses still parses.

- **`deploymentCommercialSchema`** (`/internal`, strict): `availabilityScope`
  (`availabilityScopeSchema`), `commercialPermission`
  (`commercialPermissionSchema`) and an optional `dedicatedCapacity` (absent =
  shared). `customer_byok` if and only if `byok_only`; `public_payg` requires
  `public_resale_approved`, `wholesale_contract` or `open_weight_hosting`. It is
  the optional `commercial` member of every deployment descriptor on the signed
  `POST /internal/v1/deployments/query` and `/internal/v1/models/query`. Go:
  `contract.DeploymentCommercial` (compared field by field by the descriptor
  test, with `Validate()` for the refinements). The source is
  `configs/deployment-commercial.json` (docs/inventory.md, "Commercial facts").
  `availabilityScopeSchema` and `commercialPermissionSchema` are no longer
  `notApplicable`: Go binds their members.
- **`grantStepLimitsSchema.sameModelFailover`**: `z.boolean().default(true)`.
  `false` makes `resolveGrant` authorize ONE deployment (one provider) per
  admitted line in the step, the best-ranked (cheapest funding tier, then the
  grant's line order, then the inventory's); no other provider of that line is
  behind it, so a failure is not retried on one. Later steps are unaffected. Go:
  `GrantStepLimits.SameModelFailover *bool` (nil = true) and
  `SameModelFailoverAllowed()`. Rotation among the keys of the chosen
  deployment's pool (ADR 0001) is not a provider failover and still applies.

## Contract set 4.3.0: the declared catalogue (ADR 0002, amendment 2026-10-15)

MINOR, `@kaana.ai/contracts` 4.3.0. Additive; everything 4.2.0 parses still parses.

- **`declaredModelSchema` / `declaredCatalogueSchema`** (`/internal`): the
  catalogue Kaana declares in `configs/public-catalogue.json` — `modelSchema`
  without `schemaVersion`, `currentRevision` and `availableRevisions` (Kaana
  fills those from its inventory), plus `audience`, and strict. A declared model
  carries its list price per unit and never `servingProviders`,
  `availabilityScope` or `commercialPermission`.
- **`declaredCatalogue`** is a new member of the signed `GET /internal/v1/models`
  response: the whole file, so Oxy rates grants from Kaana's prices. Both
  schemas are `notApplicable` to the Go descriptor test: Go relays the file as
  raw JSON and `tools/contract validate` parses it with the Zod schema.

## Contract set 4.1.0: input transcription inside a realtime session (Alia ADR 0017, phase 1)

MINOR, `@kaana.ai/contracts` 4.1.0. Everything 4.0.0 parses still parses; a
consumer on 4.0.0 refuses what is new.

- **`input_transcription_milliseconds`** joins the closed unit set: the
  milliseconds of a realtime session's input audio that the deployment's own
  transcription model transcribed, where the provider bills that model apart
  from the session model (OpenAI). A second model's reading, so it sits beside
  the session model's `audio_input_tokens` without breaking the partition
  rule; a provider whose session price includes the transcription (xAI) never
  reports it. Provider-reported. `money.ts` states the rule and why the
  alternatives (`audio_input_milliseconds`, or folding it into the tokens)
  would double-charge or misprice.
- **`capabilities.realtime`** gains `inputAudioTranscription` (the flag that a
  model transcribes its input inside a conversation) and `audioFormats`
  (`{input, output}` realtime encodings). Both optional: absent is undeclared.
  A realtime model also publishes `audio` (voices with `gender` where reviewed,
  languages) with empty file formats.
- **`checkRealtimeSessionOpen`** is the realtime counterpart of
  `checkSpeechRequest`: the issues an open has against a model's catalogue
  entry, refused before a grant.
- `realtimeAudioFormatSchema` moved from `realtime.ts` to `audioFormats.ts`
  (same members, same export name from the package root), so the catalogue
  can name it.
- The grant fact `audioInputMilliseconds` documents how Kaana measures an
  upload (`internal/audioprobe`, `docs/openai-audio.md`, "Measuring an
  upload's duration").

The signed-envelope lane (`/internal/v1/realtime`) keeps refusing input
transcription: its consumer settles against a closed unit set that has no
unit for it (`docs/realtime.md`).

## Contract set 4.0.0: `@kaana.ai/contracts` (ADR 0002, phase 1)

The inference contract moved here from `@oxy.so/contracts` 4.12.0 (set 3.8.0)
and is the first set published as `@kaana.ai/contracts`, version 4.0.0. Two
entry points:

- **The root** is the public API of `https://kaana.ai/v1`: requests and
  responses of every endpoint, the response and speech streams, the public error
  vocabulary (the internal one minus `provider_credential_invalid`,
  `provider_billing_refused` and `deployment_unavailable`), the catalogue with
  prices, power classes and per-model audio capabilities (voices, languages,
  formats, `speed` range, maximum characters), power levels, transcription,
  realtime and generation records. No public shape a caller READS names a
  provider, a key or a deployment; `publicSurface.test.ts` walks every one to
  hold that. The one exception is input: the chat completions and responses
  requests carry the `provider` routing preference, which names the providers a
  caller wants (ADR 0008), and the test allows exactly those two requests.
- **`@kaana.ai/contracts/internal`** is what Oxy and Kaana exchange: the grant
  and its authorization request, the settlement, the canonical input hash, the
  operational catalogue, customer-credential control, and the signed v1–v4
  envelope with its stream and usage report, which stays the only wire until
  phase 3.

Billing, the ledger (reservations, receipts, refunds), price versions, the
routing policy as a Console resource, entitlements, Inbox and model-release
ingestion stayed in `@oxy.so/contracts`. Every shape that moved parses the same
bytes as before — the regenerated descriptor matches 4.12.0 shape for shape,
the moved Oxy suites pass, and every Go fixture parses — with two deliberate
changes: the envelope admits `audio_speech` with `stream: true` (decision 8; an
adapter still refuses what its provider cannot stream) and the economic
treatment gains `platform_fee_only`.

From contracts 8.0.1, Zod must be `>=4.6.7 <5.0.0`, including the oversized-array
validation fix in GHSA-mrhx-c7xc-m3w5. Zod 4.5 changed string bounds to code points;
the contract, Go and frozen cross-language fixtures still measure UTF-16 code
units. String schemas therefore use explicit `minUtf16Length`, `maxUtf16Length`
and `utf16Length` checks from `src/stringBounds.ts`, preserving the standard
bound metadata consumed by both generators. These are local check instances;
Zod itself and array validation remain unchanged. `zodSemantics.test.ts` covers
astral characters, composition, JSON Schema bounds and early oversized-array
rejection.
The UTC timestamp check also retains the prior minute/second/fractional
precision grammar explicitly; Zod's newer default requires seconds. Generated
OpenAPI may use the equivalent JSON Schema type array for nullable strings.

## Funding tiers, contract set 3.8.0

Contract set 3.8.0 (then `@oxy.so/contracts` 4.12.0) added
`inferenceFundingTierSchema` (`free`, `credit`, `paid`), an authorized route's
optional `maxFundingTier` and the usage report's optional `fundingTier` (Oxy
ADR 0038, ADR 0001). It moved into `@kaana.ai/contracts` 4.0.0 unchanged.

## Decisions foundation

Typed decisions (contract set 3.5.0, envelope 2) came from the Oxy contract
package and moved here with the rest in 4.0.0. The contract is now authored in
this repository: `descriptor.json` and `openapi.json` are generated from
`packages/contracts` and CI fails on any diff. `generate.mjs` refuses a build from
an unrecorded source. See [decisions.md](decisions.md). Publishing the contract
enables nothing: no production route is enabled by a version bump.
The historical rollout notes below predate this foundation.
