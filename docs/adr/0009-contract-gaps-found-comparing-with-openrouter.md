# ADR 0009 — Contract gaps found comparing Kaana with OpenRouter

- Status: proposed. Every decision below widens a shared contract, so each needs
  the owner's decision and, where stated, Oxy's half first.
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

## Decisions (all proposed)

1. **Cache write as its own unit** (needs Oxy first). The cache-write premium
   (Anthropic charges 1.25x for a five-minute write and 2x for a one-hour one, per
   OpenRouter's caching guide) is folded into `input_tokens` today
   (`architecture.md`, finding 5), so a cached prompt's first request is
   under-priced and the customer sees no cache control at all (no `cache_control`
   on the request). A separate `cache_write_input_tokens` unit is the honest
   shape. `USAGE_UNITS` is closed and settlement prices every reported unit
   (`money.ts`), so a unit Oxy's ledger and price versions do not know would be
   refused or mispriced: Oxy ships the unit and its price first.
2. **Reasoning continuity** (needs the owner). `messages.ts` omits reasoning on
   purpose, because an opaque per-block blob "needs a home nobody has chosen yet".
   Anthropic thinking blocks and their signatures must be sent back unmodified
   across tool turns; OpenRouter does it with an ordered `reasoning_details`
   array. Dropping them is a correctness gap for tool use on thinking models.
   What is to be decided is where the opaque block lives and that it is never
   rendered as answer text. A reasoning token budget and an `exclude` flag are
   smaller additions on the same surface.
3. **An Anthropic Messages dialect** (needs Oxy first). Serving `/v1/messages`
   lets Claude Code and similar clients change only a base URL. `APIFormat` is a
   closed enum that Oxy authorizes by, so Oxy learns the format first.
4. **Capabilities with three states** (needs the owner). `structuredOutput`,
   `jsonMode` and `promptCaching` are true on 1 model of 260, because the flag
   means "observed". A reader cannot tell "no" from "not recorded". The catalogue
   rule (primary evidence only) stands; the schema needs a way to say unknown.
5. **Response healing for `json_schema`** (needs the owner). Repairing nearly
   valid JSON (a missing bracket, a markdown fence) without a retry, non-streaming
   only, as OpenRouter's plugin does. It rewrites model output, which Kaana does
   nowhere else, so it is opt-in per request or not at all.

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
- Reasoning is never delivered as a `text` part.
- Nothing public names a provider.
