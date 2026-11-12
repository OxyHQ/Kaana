# ADR 0009 — Contract gaps found comparing Kaana with OpenRouter

- Status: proposed, except decision 2, which the owner accepted and which is
  implemented (contract set 8.6.0), and decision 5, which the owner authorized
  and which is accepted and implemented (contract set 8.9.0). Every other decision below widens a shared
  contract, so each needs the owner's decision and, where stated, Oxy's half
  first.
- Date: 2026-11-12
- Companion: [ADR 0008](0008-provider-selection-lessons.md) covers provider
  selection (capability-aware routes, measured tie-breaks, cache affinity, caller
  preferences). This ADR covers what 0008 does not: usage units, reasoning,
  dialects and capability flags.
- Sources: OpenRouter's documentation read 2026-11-12
  (`/docs/guides/best-practices/{prompt-caching,reasoning-tokens}`,
  `/docs/api-reference/{errors,streaming}`, `/docs/guides/features/structured-outputs`,
  the Anthropic Messages reference). Pages were summarised by a model, not read in
  full: re-read each at its source before a decision rests on a figure.

## Context

A comparison with OpenRouter, Kaana's dominant upstream (338 of the 354
deployments in `configs/inventory.json` are reached through it), found gaps that
are not routing. Each is a place where Kaana's closed contract says less than the
providers behind it, so a customer cannot do something the model can.

What Kaana already does at least as well, and keeps: a signed authorization per
request, no amount in any response, a cost row for every attempt, BYOK custody
with no read route, an exact model reference that is never substituted, and no
prompt in any log.

## Decisions

1. **Cache write as its own unit** (needs Oxy first). The cache-write premium
   (Anthropic charges 1.25x for a five-minute write and 2x for a one-hour one, per
   OpenRouter's caching guide) is folded into `input_tokens` today
   (`architecture.md`, finding 5), so a cached prompt's first request is
   under-priced and the customer sees no cache control at all (no `cache_control`
   on the request). A separate `cache_write_input_tokens` unit is the honest
   shape. `USAGE_UNITS` is closed and settlement prices every reported unit
   (`money.ts`), so a unit Oxy's ledger and price versions do not know would be
   refused or mispriced: Oxy ships the unit and its price first.
2. **Reasoning continuity** (accepted; the owner reverted the earlier omission:
   "forget what was before, improve everything"). Implemented in contract set
   8.6.0. `messages.ts` had left reasoning out because an opaque per-block blob
   "needs a home nobody has chosen yet"; the cost was correctness, since Anthropic
   requires its thinking blocks and signatures back, unmodified and in order,
   across tool turns, and OpenRouter requires its ordered `reasoning_details`.
   The design:
   - **Control.** `reasoning: {effort?, maxTokens?, exclude?}`; `effort` and the
     thinking budget `maxTokens` are alternatives; `exclude` withholds what the
     model still reasons (and bills). `reasoning.maxTokens` is a request-parameter
     word; each adapter states what it accepts and refuses the rest by name
     without naming a provider (ADR 0003): Anthropic sends `thinking.budget_tokens`
     only to the models documented to take it (Opus 4.5, Haiku 4.5, and the
     deprecated-but-working Opus 4.6 and Sonnet 4.6; current models answer it
     with a 400), OpenRouter sends `reasoning.max_tokens`, direct OpenAI-compatible
     providers refuse it. `exclude` is applied once, in the emitter, for every
     route, so no route states it.
   - **The block.** A new assistant-only content part `{type: 'reasoning', text?,
     opaque?}`. `opaque` is a sealed, provider-family-tagged token the customer
     returns unmodified; the public shape names no provider. An adapter forwards
     only its own family's blocks, in order, and drops the rest; where the
     provider requires thinking blocks (Anthropic with a thinking budget on a tool
     turn) and none can be sent, the request is sent without thinking rather than
     with an invalid sequence. Sealing is not cryptographic: a forged block only
     gets the customer a provider 400.
   - **Streaming.** The `reasoning` delta keeps carrying text and gains an
     optional `opaque`, once per finished block, only for a request that carried
     `reasoning`, so older readers never see it.
   - **Surfaces.** `/v1/responses` and `/v1/messages` (thinking blocks, the
     signature being the token). Chat completions carries none: no standard field.
   - **Boundaries kept.** Reasoning is never delivered as a `text` part; no
     reasoning block, token or signature is persisted or logged; nothing public
     names a provider.
3. **An Anthropic Messages dialect** (needs Oxy first). Serving `/v1/messages`
   lets Claude Code and similar clients change only a base URL. `APIFormat` is a
   closed enum that Oxy authorizes by, so Oxy learns the format first.
4. **Capabilities with three states** (needs the owner). `structuredOutput`,
   `jsonMode` and `promptCaching` are true on 1 model of 260, because the flag
   means "observed". A reader cannot tell "no" from "not recorded". The catalogue
   rule (primary evidence only) stands; the schema needs a way to say unknown.
5. **Response healing for JSON formats** (accepted; the owner authorized the
   contract widening; implemented in contract set 8.9.0). Repairing nearly valid
   JSON (a markdown fence, prose around the value, a trailing comma, closers a
   model forgot) without a retry, non-streaming only, as OpenRouter's plugin
   does. It rewrites model output, which Kaana does nowhere else, so it is
   opt-in per request. The design:
   - **Control.** `responseFormat.repair?: boolean` on `json_object` and
     `json_schema` of `POST /v1/responses`; default off, byte-for-byte the old
     behaviour. Refused as `invalid_request` naming `responseFormat.repair` on a
     `text` format and with `stream: true`. Chat completions and `/v1/messages`
     carry no such member: a standard client has no typed field for it, and the
     Messages dialect has no JSON format at all.
   - **Kaana-side.** It runs on the final assembled text, in the public API fold,
     never in an adapter. It needs nothing from a provider, so it is not a
     request-parameter word, does not narrow routing and is not in the signed
     request or the grant.
   - **Only a finished answer.** Only when the finish reason is `stop`: an answer
     cut by the token limit (`length`), a filter, a refusal or a tool call is
     never touched (the truncation OpenRouter's plugin also cannot repair).
   - **Deterministic, conservative.** `internal/jsonrepair`: trim, strip one code
     fence, extract the first balanced object or array from prose, drop trailing
     commas, close an unterminated string and brackets at the end. Text that
     parses is never altered; text no step makes valid is returned untouched.
     Nothing is invented or reordered. There is no schema validation: Kaana
     carries no validator.
   - **Marker.** The response header `Kaana-Repaired: true`, never a body member,
     so every older reader parses the answer. Usage and cost are the provider's
     and unchanged. No text is logged; only a count line with the request id.
   Details: [json-repair.md](../json-repair.md).

## Not adopted

- **A per-provider endpoint table on the public API.** OpenRouter publishes
  uptime, latency and price per provider because its customers choose providers.
  Kaana's customers choose models and the public surface does not name providers
  (ADR 0002 decision 10, ADR 0003). If anything is published it is per model.
- **Hosted tools, plugins and an agent loop** (web search, `apply_patch`,
  subagents). They are Alia's (`architecture.md`).
- **Control-plane features**: public rankings, app attribution, guardrails,
  workspaces, presets, budgets and fees belong to Oxy.

## Consequences

- Nothing here changes behaviour. Recording the order matters: a Kaana release
  that reports a unit or an API format Oxy does not know fails settlement, and
  merged is not deployed.

## Rules a reviewer applies

- A new usage unit or `APIFormat` is never added to Kaana before Oxy accepts it.
- Reasoning is never delivered as a `text` part, and an opaque reasoning token is
  never logged, persisted or parsed outside `internal/reasoningblock`.
- Nothing public names a provider.
