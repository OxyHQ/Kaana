# Typed decisions (dormant)

Oxy owns the public `/v1/decisions` request, eligibility, reservation, immutable
model and policy selection, and any bounded queue of individual jobs. Kaana
accepts the existing signed envelope at `POST /internal/v1/decisions`, using the
same exact-body Ed25519 verification and the sole signed origin
`https://kaana.ai`. It returns `DecisionResult` JSON with a completed technical
usage report, or a failure as described under "Failures". There is no provider
batch endpoint, new ledger, local policy evaluator, or generation reservation.
A classification and a later generation are separate requests and receipts.

Contract authority is Oxy's decisions foundation, contract set **3.5.0**, envelope
**2**. Choice arrays preserve the requested option order; score distributions
preserve zero-based level order and their mean must match the distribution;
Choice and Score preserve the actual provider reply and confidence as separate
signals; missing confidence fails closed. Noul is one proposition probability. Answers match each exact question id once.
All probabilities are finite and bounded, with 1e-6 sum/mean tolerance. Input is
text only, at most 255 questions. Input budgets restate Oxy's
`decisionInputBudget` byte for byte: the larger of the normalized payload and
the provider's structured-question body, serialized with JSON escaping and a
255-byte model allowance, with shared instructions repeated per question.
`total ≤ 64000` and `context ≤ 32000` (state plus the largest single question);
a gateway additionally needs `total + 4096 ≤ 32000`. Go's encoder escapes
`< > & U+2028 U+2029` exactly as Oxy's measurement does, and a test pins Go's
numbers to Oxy's on the same inputs. Chat controls, streaming, tools and
unpinned targets are refused. No confidence is synthesized.

## Provider mapping

The TypeSafe slug is `typesafe`, using only
`https://api.typesafe.ai/v1/systemone` and `jev-1.13.0`. Its adapter is the
provider-configured `openaicompat.Adapter` with decisions as its only family;
it never accepts chat or probes health with ordinary credentials. It has no
built-in discovery/startup entry or published deployment.

OpenRouter remains the existing `openrouter` adapter and canonical
`https://openrouter.ai/api/v1` base, with the family endpoint `/systemone`.
Neither another slug nor an alternative base can borrow that origin. The
candidate `typesafe/jev-1.13-20260917` is confined to dormant wire translation;
the published example returning that id does not establish its eligibility as
an immutable request id. `typesafe/jev-1.13`, latest aliases, and opaque Jev
routers are not inventory authority.

Each question becomes a named System One question. Common instructions, the
question, and its optional rubric remain distinct structured instruction fields.
Choice labels become criteria-map keys, and score levels become ordered criteria.
Shared instructions repeat per question on this wire. The body is built from
`contract.DecisionWireQuestions`, the same representation the budget measures,
and translation bounds the final encoded body again before sending: ≤ 64000
whole and ≤ 32000 per single question, and on OpenRouter the questions plus the
4096-byte policy allowance ≤ 32000 with the policy inside that allowance. A
refusal is `request_too_large`, before any spend. Explicit effort is refused: the reviewed HTTP reference supplies no effort
field. The contract can represent future efforts without the adapter guessing.

OpenRouter's wire policy explicitly sends `only` and `order` containing only
`TypeSafe`, empty `ignore`, `allow_fallbacks:false`, `data_collection:"deny"`,
`require_parameters:true`, `zdr:true`, and `max_price` with zero prompt/completion
ceilings. Zero spend is the deliberately closed ceiling until price authority
is reviewed; no customer price or policy value is invented. Whether this family
actually honors every preference, including ZDR, needs independent evidence.

The adapter checks returned model and, on OpenRouter, its TypeSafe provider and
nonempty upstream id. Public result identity comes from Oxy's request and the
exact signed route, never an upstream id or alias. A response member the Go
decoder could read two ways is refused: an exact duplicate anywhere, and in a
struct-decoded object (the response, `usage`, each answer) a member not spelled
exactly or two members equal under Unicode simple folding, which is how
`encoding/json` matches fields. Map keys (question ids, choice labels) stay
exact, so ids `A` and `a` remain two answers.

What a failed response measures depends on how far it parsed. A body whose
leading JSON object cannot be decoded (cut, truncated, malformed) measures no
units. Once that object decodes, it records `requests: 1`, its non-negative
token counters and its cost, even when an answer is invalid or trailing bytes
follow it. If `usage` itself is ambiguous, its token counters
and cost are withheld, and only `requests: 1` is recorded.

`usage.cost` is parsed only by `providercost`, attached only to the operator
attempt, and never enters the decision response. It is either the exact amount
at 1e-12 scale — exponent forms included, `1.5e-7` is exact — or unknown: absent,
`null`, non-numeric, negative, or finer than the scale is unknown, never zero
and never rounded, and an unknown cost never discards a valid paid answer.
Provider confidence is returned separately from probabilities. Upstream ids and
answer content are not persisted or logged. Requests have a 30-second upstream
deadline as well as caller cancellation; a complete, validated answer read
before a late deadline or cancellation lands still settles as completed.

## Failures

Oxy reads three answers, and Kaana gives exactly one of them:

- **4xx, bare `InferenceError`**: refused before any provider attempt, so
  nothing executed — 400 invalid request, 413 `request_too_large`, 403
  permission (including the dormant gates), 404 model, 429 a customer-key
  throttle. Capacity with nothing attempted (no route) is a typed 503
  `DecisionFailure` without usage.
- **502, `DecisionFailure`** `{schemaVersion:1, requestId, error, usage?}` once
  any attempt reached an adapter. `error.retryable` is always false (narrowed
  by `Error.WithoutRetry`; never widened): the body may have run and nothing
  retains it. `usage` is the incomplete report, present only when the provider
  measured units; absent means unmeasured, never zero. A cut connection, a
  deadline and a malformed or truncated body are unmeasured. A parsed body
  with an invalid answer carries `requests: 1` and whatever unambiguous
  counters it reported.
- **200, `DecisionResult`**.

A decisions attempt is never retried on its route nor failed over (see
`routing.md`). Inside one attempt the credential walk re-sends only after a
definitive 401/402/429 refusal, which accepted nothing; a transport failure
returns at once.

## Closed gates and release dependencies

**Unpaired UTF-16 surrogates are an open activation gate.** Go decodes a lone
surrogate escape such as `"\ud800"` in the signed envelope to U+FFFD. The
provider would therefore receive different text from what Oxy accepted, and
Oxy's measurement of the escape (6 bytes) differs from Kaana's (3). The size
difference cannot let Kaana accept anything Oxy refused, and the budgets stay
as they are. The silent substitution is still unacceptable for a decision.
Before any gate opens, text containing unpaired surrogates must be refused as
unsupported, before spend: by Oxy's schema, and by Kaana on the raw signed
body, because after decoding it cannot be told apart from a genuine U+FFFD.
This change does not implement that refusal.

All production constructors leave five independent reviews unapproved: resale
rights, internal eligibility, privacy, ZDR, and immutable route identity. Both
translation and execution refuse while any is missing. There is no environment
flag or operator configuration to approve these reviews. Synthetic tests affirm
them only on their own fake adapters. Provider-family classification also blocks
all TypeSafe/OpenRouter Jev rows from publication. No production inventory,
credentials, public service or live call is enabled by this change.

Ordinary TypeSafe standalone resale and OpenRouter resale/competitor terms do
not supply authorization. Internal use is also blocked until its separate
eligibility/privacy review. A later change must provide exact evidence and
independent review before opening any gate, including affirmative Oxy catalogue
`apiFormats:['decisions']` qualification; missing formats grant nothing.

The descriptor is generated from an unpublished local build of the exact Oxy
commit named in its `source` field (package 4.7.0, contract set 3.5.0); see
`contract.md`. The contracts source of every later Oxy commit reviewed, through
`54335fa1c9ccc69f8ce776fa3a1d27a42959a33b`, is identical to it (`decisions.ts`
SHA-256 `2019f904c6f418b05b1925447b56cf2bfe5b577f45b404c354d630bdcd7e5083`). **This is not a published contract upgrade**, and the 4.5.0
tooling pin is deliberately unchanged until Oxy releases 4.7.0. Health reports
3.5.0, so the deployed Oxy handshake must require 3.5.0; envelope version alone
is not negotiation. Do not merge or deploy while this publication gate is open.

Reviewed public references (2026-10-01; no authenticated provider calls):

- [TypeSafe HTTP API](https://docs.typesafe.ai/api): request, answer and token fields.
- [TypeSafe model documentation](https://docs.typesafe.ai/models): version identity and context limits.
- [OpenRouter's Jev guide](https://openrouter.ai/blog/insights/what-is-jev/): compatible `/v1/systemone` path and dated response example.
- [OpenRouter provider routing](https://openrouter.ai/docs/features/provider-routing): routing preference vocabulary; family-specific enforcement remains unverified.
