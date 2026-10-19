# Typed decisions

`POST https://kaana.ai/v1/decisions` asks a decisions model a set of typed
questions about one input and answers each with probabilities. It is an
ordinary Kaana operation: the caller's credential goes to Oxy for a grant
(`operation: decisions`, docs/adr/0002), Kaana executes inside the grant on the
routes it resolves, and settles exactly once. There is no other way in: the
signed envelope lane (`POST /internal/v1/inference`) refuses a decisions
envelope, and the retired `POST /internal/v1/decisions` route is gone. Nothing
about decisions is approved in source — no compiled one-off permit, no
exact-route grant, no canonical input hash. What a decisions request may cost
and where it may run are the grant's, as for every other operation.

Contract: `@kaana.ai/contracts` 7.0.0, contract set 7.0.0
(`public/decisions.ts`). `@kaana.ai/sdk` 3.2.0's `decisions.create` refuses an
answer that does not match the request it sent (`decisionAnswersMatch`, and the
pinned model when one was named) with `KaanaProtocolError`.

## The request

- **`model` is optional.** Present, it must be revision-pinned
  (`publisher/model@revision`) and only that revision answers; it is never
  substituted. Absent, Kaana asks Oxy for a grant with no target and Oxy answers
  with the application's decisions profile as ordered steps, one per model
  line. The first line Kaana can serve answers; the success's `model` names the
  revision that did, so a consumer analyses results per `model`.
- Questions are `choice` (2–255 options), `score` (2–10 levels, ordered low to
  high) and `noul` (a yes/no proposition). Ids are unique; options and levels
  are unique within a question. Input is text only, at most 255 questions.
  Effort is refused by every adapter: neither provider documents an effort
  field. Chat controls, streaming and tools are refused.
- Input budgets restate Oxy's `decisionInputBudget` byte for byte: the larger
  of the normalized payload and the structured-question body, serialized with
  JSON escaping and a 255-byte model allowance, shared instructions repeated
  per question. `total ≤ 64000` and `context ≤ 32000` (state plus the largest
  single question); a gateway additionally needs `total + 4096 ≤ 32000`. Each
  adapter bounds its final body again before sending; a refusal is
  `request_too_large`, before any spend.
- Kaana rejects unpaired UTF-16 surrogates in the raw body before decoding
  (`contract.ValidateDecisionJSONUnicode`), so a lone surrogate cannot silently
  become U+FFFD.

## The answer

Choice probabilities follow the requested option order; a score's
distribution follows the zero-based level order and its mean must match the
distribution; a choice and a score carry the provider's reply and confidence
as separate signals, and a missing confidence fails closed. A noul is one
probability. Every probability is finite and bounded, with the contract's 1e-6
sum and mean tolerance. Upstream ids, answer content and prompts are never
persisted or logged.

## Price: every decisions route has a rate card

Before the first attempt, the executor reads each authorized decisions route's
price ceiling from its reviewed rate card in `configs/provider-rates.json`
(`providercost.Cards.DecisionPriceLimit`): a positive input-token rate and a
zero output-token rate, since both providers bill decisions by input tokens. A
route without such a card is not served — never priced at zero — and when no
authorized route has one the request fails `no_route_available` with nothing
attempted. Every adapter also refuses a decisions call that reaches it without
a ceiling.

## Providers

### TypeSafe's Jev on OpenRouter (`openrouter`, `/systemone`)

The existing OpenRouter adapter and canonical base
`https://openrouter.ai/api/v1`, family endpoint `/systemone`. Only `typesafe/`
models answer it. Each question becomes a named System One question; common
instructions, the question and its optional rubric stay distinct structured
fields; choice labels become criteria-map keys and score levels ordered
criteria. The body is built from `contract.DecisionWireQuestions`, the same
representation the budget measures.

The wire policy is fixed: `only` and `order` contain only `TypeSafe`, empty
`ignore`, `allow_fallbacks: false`, `data_collection: "deny"`,
`require_parameters: true`, `zdr: true`, and `max_price` is the route's rate
card (prompt per million input tokens, completion 0). Whether OpenRouter honours
every preference on this surface still needs independent evidence; a refusal is
kept, never answered by removing a privacy filter.

The publisher publishes Jev as an ordinary OpenRouter deployment. OpenRouter
lists it under the alias `typesafe/jev-1.13`, and its zero-retention list names
that alias with the TypeSafe endpoint; the row's own `canonical_slug`
`typesafe/jev-1.13-20260917` names the dated weights, and `/systemone` accepts
it and answers with it (2026-10-05). So the publisher replaces a `typesafe/`
row's alias with its dated canonical slug after the zero-retention filter, and a
row with no dated slug of its alias is unservable
(`publisher.pinTypeSafeToCanonicalSlugs`). The attribution is
`openrouter: typesafe/jev-1.13-20260917 → typesafe/jev-1.13`, so the inventory
mints, like every other line, the reference
`typesafe/jev-1.13@observed-<first date seen>` and the deployment
`dep_openrouter_typesafe_jev_1_13_20260917_observed_<YYYY_MM_DD>`. The
deployment serves nothing until its id has a rate card and a commercial entry
(`configs/deployment-commercial.json`, `platform_internal`).

The adapter checks the returned model, TypeSafe as the serving provider and a
nonempty upstream id. A response member the Go decoder could read two ways is
refused: an exact duplicate anywhere, and in a struct-decoded object (the
response, `usage`, each answer) a member not spelled exactly or two members
equal under Unicode simple folding. Map keys (question ids, choice labels) stay
exact. TypeSafe computes a score on the normalized mass of probabilities that
may carry a rounding residual, and prints its answers at two decimals
(TypeSafe's published examples and every production answer). An answer whose
score and probabilities are all on that grid is held to the precision it was
printed in: the mass within half a hundredth per level, and the score within
half a hundredth plus |level − mean| × half a hundredth per level, over the
mass, of the normalized mean; the answer then carries that mean, as the OpenAI
adapter's does. Any answer off the grid has its mass held within 1e-6,
normalized, and keeps TypeSafe's score, which must match the mean within 1e-6.
OpenRouter's moderation refusal (403 naming reasons) is
`upstream_content_filtered`, unmeasured.

### OpenAI Decisions API (`openai-decisions`, `/decisions`)

OpenAI's Decisions API (public beta since 2026-10-07,
https://developers.openai.com/api/docs/guides/decisions), `POST /v1/decisions`,
model `gpt-6-luna`, on its own locked slug: `gpt-6-luna` is a chat model under
`openai` and OpenRouter, and a slug resolves to one adapter family, so the
decisions model cannot share `openai`'s classification. The slug also gives
decisions their own exact key, which is what OpenAI's per-account zero data
retention has to be evidenced on.

Mapping (`openaicompat/decisions_openai.go`): `input` is the state; each
question is `{type, name, instructions}` with `noul → predicate`,
`choice → choice` with `choices: [{value, description}]` and
`score → score` with `levels: [{label, description}]` low to high. The
contract names an option or a level by its text alone, so that text is both its
value or label and its description; nothing is invented beside it. A
question's `instructions` are the shared instructions, the question and its
rubric, in that order, joined by a blank line.

The answer is keyed by `name`. Probabilities are reordered into contract order
(a choice by `value`, a score by `label`, with `value` as the zero-based index
when it is a number). The distribution's mass must be one within 1e-6 and is
normalized. OpenAI's score is the probability-weighted mean of the level
indices and may be printed rounded: it must agree with the distribution's mean
to the precision it was printed in (half its last printed digit, plus 1e-6),
and the answer then carries that mean, as Jev's normalized mass is carried. A
`refusal` answer is `upstream_content_filtered` with the input tokens OpenAI
measured. Usage is `usage.input_tokens` (required) and `output_tokens` if
reported; OpenAI reports no cost, so the rate card prices the attempt.

Status: **built and conformance-tested, served by nothing.** There is no
`openai-decisions` key, attribution, deployment, rate card or commercial entry,
so no inventory row exists and no grant can name one. Activating it needs: an
OpenAI key with credit and evidenced zero data retention for that account,
imported under `openai-decisions`; the attribution (and with it the model line:
whether the Decisions API's `gpt-6-luna` is the same weights as the chat
`openai/gpt-6-luna`, which a shared line would mix with chat deployments); a
public-catalogue entry with that evidence; the rate card ($0.10 per million
input tokens, 2026-10-07) and the commercial entry; then Oxy's decisions
profile. OpenAI documents no rate limits or idempotency for this API.

## Failures

A failed public request is logged as `inference request failed` with its code
and its upstream attempt count; an answer either adapter refused also carries
the finite `decisionResponseValidation` reason
(`provider.InvalidDecisionResponse`), never the provider's text.

A refusal before any provider attempt is a public 4xx (invalid request,
`request_too_large`, permission, model) or `no_route_available`, and nothing
executed. Once an attempt reached an adapter the request may have been accepted
and billed: its failure is never retryable, and the settlement carries what the
provider measured — a parsed body with an invalid answer records `requests: 1`
and its counters; a cut connection, a deadline and a malformed or truncated
body are unmeasured, never zero. If `usage` itself is ambiguous its counters are
withheld and only `requests: 1` is recorded. OpenRouter's `usage.cost` is parsed
only by `providercost`, attached only to the operator attempt, never to a
response: exact at 1e-12 or unknown, never rounded, and an unknown cost never
discards a valid paid answer.

A decisions attempt is never retried on its route nor failed over after it
was sent. Inside one attempt the credential walk re-sends only after a
definitive 401 or 402 refusal (or a throttle, on keys of separate accounts),
which accepted nothing; a transport
failure returns at once. Routes the executor skips before sending (an open
breaker, no key within the funding ceiling, no rate card) are not attempts, so
an unpinned request moves to the profile's next line exactly then. Requests
have a 30-second upstream deadline as well as caller cancellation; a complete,
validated answer read before a late deadline or cancellation lands still
settles as completed.

## The grant's input estimate

Kaana asks Oxy for the grant with `facts.estimatedInputTokens` = a quarter of
the request body's bytes. A provider bills its own prompt framing on top:
TypeSafe's API examples bill 296 input tokens for a one-question request of
about 150 bytes, and Mention's production requests (2026-10-18, four
questions) bill the estimate plus 330–370 tokens, about 2.5 times it. The
estimate is therefore not a bound on what Jev bills. Oxy grants decisions only
to internal relationships, which hold nothing, so no hold is undersized today;
a commercial decisions grant needs a sound input bound first.

## Conformance

`conformance.RunDecisions` holds every decisions adapter to the same
behaviour through the real executor on one priced deployment: a complete answer
in contract order, a refusal and an invalid answer that keep what the provider
measured, a cut connection that measures nothing and is not re-sent, a 401 and
a 402 on the first key answered by the second, a cancellation the provider
observes, and a route without a rate card that is never called. Both adapters
run it against fakes of their real wires
(`openaicompat/decisions_conformance_test.go`).

## References

- [OpenRouter's System One guide](https://openrouter.ai/docs/guides/community/typesafe-sdk):
  `/api/v1/systemone` and a response naming `typesafe/jev-1.13-20260917` served
  by `TypeSafe`.
- [OpenRouter's zero-retention endpoints](https://openrouter.ai/api/v1/endpoints/zdr):
  `TypeSafe | typesafe/jev-1.13-20260917` under `typesafe/jev-1.13`, prompt
  `0.000000042` USD/token, completion 0 (read 2026-10-04).
- [OpenRouter provider routing](https://openrouter.ai/docs/features/provider-routing):
  the preference vocabulary; enforcement on `/systemone` is unverified.
- [TypeSafe HTTP API](https://docs.typesafe.ai/api) and
  [models](https://docs.typesafe.ai/models).
- [OpenAI Decisions guide](https://developers.openai.com/api/docs/guides/decisions).
