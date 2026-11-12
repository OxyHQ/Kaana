# Changelog

## 8.6.0

`POST /v1/messages` accepts `thinking: {type: "adaptive"}` and
`{type: "enabled", budget_tokens}` (at least 1,024), so a client that turns
extended thinking on is no longer refused. Additive: everything 8.5.0 parses
still parses; the signed grant and envelope are unchanged.

- The contract's reasoning control is an effort, not a token budget, so a budget
  is mapped to the nearest effort (under 4,096 `low`, under 16,384 `medium`,
  otherwise `high`) and is not enforced as a cap. `output_config.effort` wins
  over a budget; `adaptive` and `disabled` ask for nothing.

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
