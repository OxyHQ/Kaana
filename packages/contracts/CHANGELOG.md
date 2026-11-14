# Changelog

## 8.17.0

The catalogue in the caller's contract set. No shape, field or member changes:
everything 8.16.0 parses still parses.

- `CATALOGUE_ADDITIONS`: each value `GET /v1/models` gained after a set, with the
  set that introduced it (`capabilities.unreported` and the unit price
  `cache_write_input_tokens` in 8.4.0; the session kind `live` and
  `capabilities.realtime.defaultKinds` in 8.16.0). Kaana renders the catalogue in
  the set a request states in `Kaana-Contract-Version`, leaving out every value
  that set lacks, so a reader pinned to an older set keeps parsing the whole list.
  A request that states no set receives none of them.

## 8.16.0

Live voice sessions with delegation (Kaana ADR 0012). Additive: everything
8.15.0 parses still parses. No line serves `live` yet; the adapters follow.

- `realtimeSessionKindSchema` gains `live`: a spoken conversation whose turns the
  caller's backend answers.
- `realtimeSessionConfigSchema` gains the optional `delegation`
  (`realtimeDelegationConfigSchema`: `acknowledgement` `auto` or `none`),
  required for `live` and refused for every other kind. A `live` session also
  needs a `voice`, one audio format both ways and a turn detection other than
  `none`, and refuses `outputModalities`, `tools`, `toolChoice`,
  `maxOutputTokens`, `translation` and an input transcription `prompt`
  (`realtimeConfigIssues`).
- Commands `delegation.reply` (`delegationId`, `text` of 1 to 8192 UTF-16 units,
  `verbatim?` of at most 32 spans of 1 to 256 units, each a part of `text`,
  `final`), `delegation.context` (`delegationId`, `text`) and `delegation.cancel`
  (`delegationId`). `MAX_REALTIME_DELEGATION_TEXT_CHARS`,
  `MAX_REALTIME_VERBATIM_SPANS` and `MAX_REALTIME_VERBATIM_SPAN_CHARS` state the
  bounds.
- Events `delegation.requested` (`delegationId`, `text`) and
  `delegation.superseded` (`delegationId`), in the public and the signed event
  unions.
- `capabilities.realtime.defaultKinds`: the session kinds a line is Kaana's
  default for, each one of its `sessionKinds`. `declaredCatalogueSchema` allows
  at most one active default line per kind.

## 8.15.0

The rerank request's documents in the signed envelope. Additive: everything
8.14.0 parses still parses, and the public rerank shapes and the grant are
unchanged.

- `inferenceRequestSchema` gains the optional `rerank` member
  (`inferenceRerankParametersSchema`: `documents`, `topN`, `returnDocuments`). It
  is required with, and valid only with, the `rerank` API format, text modality, a
  non-empty text query and no streaming; a decisions envelope carries none.

## 8.12.0

Text-to-image controls in the signed envelope. Additive: everything 8.11.0
parses still parses, and the public request and response shapes are unchanged.

- `inferenceRequestSchema` gains the optional `image` member
  (`inferenceImageParametersSchema`: `n`, `size`, `quality`, `style`,
  `responseFormat`). It is valid only with the `images_generations` API format,
  image modality and text input, and a decisions envelope carries none.

## 8.11.0

The `rerank` operation, contract only. Additive: everything 8.10.0 parses still
parses. Kaana does not serve `POST /v1/rerank` and emits no `rerank` grant
operation yet; Oxy supports the operation first.

- `grantOperationSchema` gains `rerank`, appended last (Oxy compares the list in
  order).
- `rerankRequestSchema`: `model`, `query`, `documents` (1 to 1000 non-empty
  strings; string documents only), `top_n?`, `return_documents?`, `user?`.
- `rerankResponseSchema`: `id`, `model`, `results` (`index`, `relevance_score`,
  optional `document.text`), `usage` (`prompt_tokens`, `total_tokens`). No amount.
- No new usage unit: `requests` and `input_tokens` suffice.

## 8.10.0

Price controls in the `provider` routing preference (Kaana ADR 0008). Additive:
everything 8.9.0 parses still parses, and the signed grant and envelope are
unchanged.

- `provider.max_price`: `{ prompt?, completion? }`, USD per million tokens, at
  least one. It is checked against the DECLARED LIST PRICE of a model line (the
  price Oxy rates the grant at), so it decides which line may serve the request:
  a line over it, or one whose price is not stated, is not used, and a request it
  leaves with no line is refused.
- `provider.sort`: `"price"` joins `"latency"` and `"throughput"`. It tries the
  model line with the lowest declared list price first (input plus output per
  million tokens); every route of one line costs the customer the same, so it
  does not reorder routes within a line.
- No response carries an amount.

## 8.9.0

Opt-in JSON repair (Kaana ADR 0009 decision 5). Additive: everything 8.8.0
parses still parses.

- `POST /v1/responses`' `responseFormat` (`responsesResponseFormatSchema`) gains
  `repair?: boolean` on `json_object` and `json_schema`. `repair: true` asks
  Kaana to repair nearly valid JSON (a code fence, prose around the value,
  trailing commas, closers a normal stop forgot) in the final text of a
  non-streaming answer that ended with `stop`. Default off; refused with
  `stream: true`. It is a public request member only: the signed grant and
  envelope (`responseFormatSchema`) are unchanged.
- A repaired answer carries the response header `Kaana-Repaired: true`
  (`KAANA_REPAIRED_HEADER`), never a body member, so no older reader is affected.

## 8.8.0

An optional prompt-cache breakpoint, `cacheBreakpoint`, on a `text`, `image` or
`file` content part and on a tool definition (not on a realtime tool). Additive:
everything 8.7.0 parses still parses and a request without it is unchanged.

- The marker means Anthropic's `cache_control: {type: "ephemeral"}` with the
  default 5-minute lifetime and nothing else. A route that cannot express it
  ignores it; the Anthropic adapter sends it (at most 4 per request, the earliest
  in prompt order: tools, system, messages). The executor sends it only to a route
  whose granted price version prices `cache_write_input_tokens`.
- `POST /v1/messages` translates a `cache_control` of type `ephemeral` without a
  `ttl` (or with `"5m"`) into the marker where the route prices the write; any
  other marker, and every marker on an unpriced route, is still ignored and named
  in `Kaana-Ignored-Parameters`.

## 8.7.0

`POST /v1/messages` accepts `cache_control`, `context_management`,
`thinking.display` and `tools[].defer_loading` and ignores them (the members
Claude Code sends), and a `role: "system"` message with text blocks in `messages`. Additive: everything
8.6.0 parses still parses; the signed grant and envelope are unchanged.

- Claude Code sends a `cache_control` marker on every request, so refusing it made
  the endpoint unusable by its main client. The marker is accepted on the request,
  `system`, a tool, a content block and a tool result's block, whatever members it
  carries, and is not honoured: no cache hint is forwarded, no cache write
  happens, and `usage` is what the request cost. A request whose marker was
  ignored is answered with `Kaana-Ignored-Parameters: cache_control`.

## 8.6.0

Reasoning continuity (Kaana ADR 0009 decision 2). Additive: everything 8.5.0
parses still parses.

- `inferenceReasoningSchema` gains `maxTokens` (a thinking budget; an
  alternative to `effort`, refused together) and `exclude`; `effort` is now
  optional, so `reasoning: {}` opts in to reasoning continuation tokens.
- `inferenceContentPartSchema` gains the assistant-only
  `{type: 'reasoning', text?, opaque?}` part. `opaque` is a sealed token the
  customer returns unmodified; it names no provider. A reasoning part on any
  other role is refused, as a refusal part is.
- The `reasoning` delta may carry `opaque`, once per finished block, only to a
  request that carried `reasoning`.
- `anthropicMessagesRequestSchema` accepts `thinking: {type: 'enabled',
  budget_tokens}` and `{type: 'adaptive'}` and `thinking` / `redacted_thinking`
  blocks on assistant messages; answers and streams carry `thinking` blocks, the
  `signature` being the continuation token.
- Oxy parses these shapes on the legacy signed lane and needs to bump its pin for
  it; the public/grant lane never sends messages to Oxy.

## 8.5.0

OpenRouter-style routing preference and prompt-cache key (Kaana ADR 0008).
Additive: everything 8.4.0 parses still parses, and the signed grant and
envelope are unchanged.

- `providerPreferencesSchema`: the optional `provider` object on
  `POST /v1/chat/completions` and `POST /v1/responses`. `order`, `only`,
  `ignore`, `allow_fallbacks`, `require_parameters`, `quantizations`,
  `sort` (`latency` or `throughput`), `preferred_max_latency` and
  `preferred_min_throughput`. It can only narrow what the grant already admits.
  `sort: "price"`, `max_price`, `zdr`, `data_collection` and percentile objects
  are refused, not dropped: pricing and data policy stay Oxy's.
- `prompt_cache_key` (chat completions) and `promptCacheKey` (responses): an
  opaque string that keeps requests sharing a prefix on one deployment while it
  is admitted and its cache is warm.

## 8.4.0

Two additive declarations; nothing emits either yet, and everything 8.3.0 parses
still parses. A consumer on 8.3.0 refuses a usage report carrying the unit and a
catalogue carrying the field, so Oxy reads them before Kaana publishes them.

- Usage unit `cache_write_input_tokens`: input tokens written to a prompt cache
  on this request, the first-request premium (Anthropic's 5-minute write is
  1.25x input). A sibling under the partition rule, never inside `input_tokens`
  or `cached_input_tokens`. One unit, the provider's default TTL only.
  Appended last in `USAGE_UNITS`.
- `capabilities.unreported` (optional): the boolean capabilities
  (`tools`, `parallelToolCalls`, `structuredOutput`, `jsonMode`,
  `promptCaching`) whose `false` is not evidence, because nobody recorded an
  answer. Unique; a listed key must be `false`. Absent means undeclared. The
  five booleans stay required.

## 8.3.0

Anthropic-Messages-compatible `POST /v1/messages`. Additive: everything 8.2.0
parses still parses, and the signed grant and envelope are unchanged (the
endpoint is served as the `responses` operation).

- `anthropicMessagesRequestSchema`, `anthropicMessageSchema`,
  `anthropicStreamEventSchema` and `anthropicErrorSchema`: an unmodified
  Anthropic SDK or Claude Code works with `ANTHROPIC_BASE_URL=https://kaana.ai`
  and an `oxy_sk_` key sent as `x-api-key` or `Authorization: Bearer`.
- Strict, and a parameter Kaana does not implement is refused, not dropped:
  `cache_control` (there is no usage unit for a cache write, so honouring it
  would under-report cost), `thinking` other than `disabled` (ask reasoning
  with `output_config.effort`), server tools, image `url` sources and every
  member the schema does not name.
- `stop_reason` is `end_turn`, `max_tokens`, `tool_use` or `refusal`;
  `stop_sequence` is never produced. `usage` carries `cache_read_input_tokens`
  only when the route measured it.
- A tool `description` may now be up to 32,768 characters (was 2,000) in
  `toolDefinitionSchema` and the Messages tool, because Claude Code's built-in
  tool descriptions exceed 2,000. Everything accepted before is still accepted.

## 8.2.0

Image input (Kaana ADR 0007). Additive.

- `checkImageInput(messages)` returns a conversation's image count and every
  issue against the image rules: inline base64 only (no image URL is fetched or
  forwarded), `INPUT_IMAGE_MEDIA_TYPES` (PNG, JPEG, WebP, GIF), at most
  `MAX_INPUT_IMAGE_BYTES` (3,750,000) decoded bytes each, on `user` or `tool`
  messages, and at most `MAX_INPUT_IMAGES` (20) a request. An issue names where
  and why, never the image. `base64DecodedLength` counts without decoding.
- `/internal`: `declaredModelSchema.imageInput` (`declaredImageInputSchema`,
  `{ maxInputTokensPerImage }`), the most input tokens one image costs on the
  line; absent, no request carrying an image is authorized on it. The grant
  fact `inputImages`, present exactly when a completion carries images.

A consumer on 8.1.0 or earlier refuses a declared catalogue or an
authorization carrying the new members: Oxy adopts 8.2.0 before Kaana declares
`imageInput` or sends `inputImages`.

## 8.1.0

Streaming transcription sessions (Kaana ADR 0006). A `transcription` session
may ask for `inputAudioTranscription.interimResults: true` and then receives
`transcript.interim { source: 'input_audio', itemId, contentIndex, text }`:
the provisional text of the current item, each one replacing the last, never
settled and never part of the transcript. `transcript.delta` still only
appends, and each item still ends in exactly one `transcript.done`. A
`conversation` or `translation` asking for interim text is refused at open.
Additive: everything 8.0.x parses still parses.

## 8.0.1

Require Zod 4.6.7 or newer in major 4 to include the oversized-array validation
fix (GHSA-mrhx-c7xc-m3w5). String bounds retain the existing UTF-16 code-unit
contract through explicit per-schema checks; the Go descriptor, wire shapes
and public limits remain unchanged.

## 8.0.0

Authenticated and grant principals are discriminated by authority. First-party
principals carry no scopes; external principals retain explicit scope arrays.
Consumers must narrow the union before reading `scopes` or `inferenceScopes`.
Historical signed records without an authority field remain external.

The Kaana reader accepts existing external grants and the new first-party shape.
Deploy that reader before enabling Oxy's first-party emitter. Signature,
audience, billing, route and account boundaries remain enforced.

## Earlier sets

Contract sets 3.8.0 to 7.0.0, published before this changelog began, and the longer reasoning behind
every set, are in [`docs/contract-history.md`](../../docs/contract-history.md).
