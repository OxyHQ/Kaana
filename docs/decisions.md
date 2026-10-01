# Typed decisions (dormant)

Oxy owns the public `/v1/decisions` request, eligibility, reservation, immutable
model and policy selection, and any bounded queue of individual jobs. Kaana
accepts the existing signed envelope at `POST /internal/v1/decisions`, using the
same exact-body Ed25519 verification and the sole signed origin
`https://kaana.ai`. It returns `DecisionResult` JSON with a completed technical
usage report, or the existing typed inference error body. There is no provider
batch endpoint, new ledger, local policy evaluator, or generation reservation.
A classification and a later generation are separate requests and receipts.

Contract authority is Oxy's decisions foundation, contract set **3.5.0**, envelope
**2**. Choice arrays preserve the requested option order; score distributions
preserve zero-based level order and their mean must match the distribution;
Choice and Score preserve the actual provider reply and confidence as separate
signals; missing confidence fails closed. Noul is one proposition probability. Answers match each exact question id once.
All probabilities are finite and bounded, with 1e-6 sum/mean tolerance. Input is
text only, at most 255 questions; conservative UTF-8 budgets are 64KiB total and
32KiB state plus instructions plus the longest whole question. Chat controls,
streaming, tools and unpinned targets are refused. No confidence is synthesized.

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
Shared instructions repeat per question on this wire; translation also bounds
that expanded text before sending. Explicit effort is refused: the reviewed HTTP reference supplies no effort
field. The contract can represent future efforts without the adapter guessing.

OpenRouter's wire policy explicitly sends `only` and `order` containing only
`TypeSafe`, empty `ignore`, `allow_fallbacks:false`, `data_collection:"deny"`,
`require_parameters:true`, `zdr:true`, and `max_price` with zero prompt/completion
ceilings. Zero spend is the deliberately closed ceiling until price authority
is reviewed; no customer price or policy value is invented. Whether this family
actually honors every preference, including ZDR, needs independent evidence.

The adapter checks returned model and, on OpenRouter, its TypeSafe provider and
nonempty upstream id. Public result identity comes from Oxy's request and the
exact signed route, never an upstream id or alias. Token counters survive an
invalid answer or billed-cost field. The exact decimal `usage.cost` is parsed
only by `providercost`, attached only to the operator attempt, and never enters
the decision response. Provider confidence is returned separately from probabilities. Upstream ids and
answer content are not persisted or logged. Requests have a 30-second upstream
deadline as well as caller cancellation.

## Closed gates and release dependencies

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

The generated descriptor was derived with the existing generator from the local
Oxy foundation build. The reviewed source identifies its prerelease package
as 4.7.0-dev.20261001.1, while this repository pins published 4.5.0. **This is not a published
contract upgrade.** Foundation must release a distinct reproducible version;
then update the exact package/lock pin, regenerate, verify zero descriptor drift,
and run `make check`. Health reports 3.5.0 so the real Oxy handshake must require
3.5.0; envelope version alone is not negotiation. Do not merge/deploy while this
publication gate is unresolved.

Reviewed public references (2026-10-01; no authenticated provider calls):

- [TypeSafe HTTP API](https://docs.typesafe.ai/api): request, answer and token fields.
- [TypeSafe model documentation](https://docs.typesafe.ai/models): version identity and context limits.
- [OpenRouter's Jev guide](https://openrouter.ai/blog/insights/what-is-jev/): compatible `/v1/systemone` path and dated response example.
- [OpenRouter provider routing](https://openrouter.ai/docs/features/provider-routing): routing preference vocabulary; family-specific enforcement remains unverified.
