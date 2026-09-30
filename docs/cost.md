# Provider cost

What a request cost Kaana upstream. Never a customer amount — that is Oxy's.

## Provider cost

What the upstream will invoice Kaana for a request. It is an **operator**
number, and this is the only package in the repository that holds an amount of
money at all.

It is deliberately not the contract's money type. ADR 0006 gives Oxy every
customer-facing amount and Kaana its own upstream cost; `internal/contract` has
no money type and must not acquire one, so the two cannot be confused by
reaching for the same struct. Nothing here appears in any produced shape: the
stream events, the usage report and the error body have no field it could
occupy, and the descriptor gate fails on any field added to them that the
contract does not have. The containment check is the same amount in two places —
present in the operator log, absent from every byte the customer receives — with
a control proving a non-zero cost was measured, so "no cost in the response"
cannot be what an unpriced request also reports.

**A failed failover attempt is off the customer's receipt and on Kaana's cost.**
The customer never received that output, so charging for it would be wrong; the
provider invoices for it regardless, so dropping it would leave Kaana
reconciling against a number short by exactly its own failover traffic. That
asymmetry is why this is a separate measurement rather than a field on the usage
report.

**An unknown cost is not a zero cost.** A deployment with no rate card, or a
unit a card does not price, produces a measurement that says so and names the
unpriced units. Summing unknowns as zero yields a reconciliation that looks
complete and is quietly short by exactly the traffic nobody priced.

Rate cards are optional (`KAANA_PROVIDER_RATES_PATH`), live in their own file
read by their own package, and are keyed by deployment id. Production's reviewed
card is `configs/provider-rates.json`, baked at
`/etc/kaana-rates/provider-rates.json` (the example file is never baked). Amounts are integers
in 1e-12 of the currency's major unit — the same scale as the published
contract's money type, so an operator reconciling an invoice against the ledger
is comparing like with like.

The file is one immutable observation, not a mutable table: schema version,
rate-card version id, provider-owned or operator-reviewed source, upstream
source version, observation time, effective time and optional expiry accompany
the deployment rates. Every estimated attempt retains that version id, so a
later price change cannot erase which observation produced the estimate.

When an upstream returns the exact amount it billed for the request, that fact
outranks the rate-card calculation for the same attempt. It is parsed directly
from the provider's decimal string into the fixed 1e-12 integer scale; it never
passes through a floating-point number. CheaperInference's
`cheaper_inference.billed_cost_usd` is the first such source. A malformed amount
fails the upstream attempt instead of silently falling back to a different
number, and providers that do not return an exact amount continue to use the
versioned rate card or report an unknown cost.

Every attempt now carries explicit operator provenance: `provider_reported`
for an exact upstream billing fact, `rate_card` for a calculated estimate, or
`unknown`. The last state has no currency or amount and therefore cannot be
summed as free traffic.

Provider-owned pricing, balance and quota APIs are collected behind
`internal/providertelemetry`. Their observations carry the opaque provider key
id, source, exact/estimated/unknown certainty, source version and freshness
window. An unavailable, stale or malformed provider response becomes an
explicit unknown observation. The controlled projection contains no plaintext
credential and is intended only for a separately authenticated operator path
into Oxy; it is never attached to an inference response.

## Per-attempt telemetry and rate-card history

Every platform-funded upstream attempt is one `provider_cost_events` row keyed
by `(request_id, attempt_index)` and bound to the exact `(provider, keyId)`,
deployment and revision-pinned model reference. Besides the cost and its
provenance, the row carries what the attempt measured about itself (migration
`0016`): its own usage units (sorted, so a replay is byte-identical), when it
started, its latency, its time to first output — timed from the attempt's own
start, so a fallback is never charged its primary's wait — and how it ended:
`succeeded`, `cancelled`, or `failed` with the contract error code it was
classified as. `rate_limited` and `provider_quota_exhausted` are how a throttle
and an exhaustion stay apart without a second vocabulary; a throttle never
retires a key. A replay that differs in any measured fact fails closed exactly
like a different amount does. Rows written before `0016` keep NULL telemetry:
they were never measured, and a zero would be a fact nobody observed.

Rate-card versions are append-only (migration `0015`). The runtime registers
the version it loaded before it serves; the same version with the same facts
is a replay, and the same `rateCardVersionId` with any different price stops
the process. `UPDATE`, `DELETE` and `TRUNCATE` on the history are refused by
trigger, and a rate-card cost that names an unregistered version is refused,
so what every estimate was calculated from can always be read back. A price
change ships as a new version id.

## The operator feed Oxy reads

Oxy owns balances, grants, spend reservations and deployment ordering. To
order on evidence it reads Kaana's measurements through two signed operator
reads on the runtime, each under its own edgeauth purpose
(`oxy-kaana-provider-telemetry:v1`): an inference signature reads nothing here
and a telemetry signature runs nothing.

- `POST /internal/v1/provider-telemetry/attempts` with
  `{"schemaVersion":1,"after":<cursor>?,"limit":1-500?}` returns attempts
  oldest first with an opaque `next` cursor and `caughtUp`. An attempt appears
  only once it is 15 seconds old: a row's position is its writing
  transaction's start, so a late commit could otherwise land behind a cursor
  the reader already passed. The upstream cost is a
  `providercost.OperatorAmount` — integer 1e-12 units as a decimal string.
- `POST /internal/v1/provider-telemetry/credentials` with
  `{"schemaVersion":1}` returns each enabled platform key's class, and when it
  has been described, its capacity category, environment, commercial-use
  eligibility, opaque funding account, restrictions and latest capacity
  evidence per kind. No account label, email, evidence text or secret is in
  it. An undescribed key has a null description, which Oxy must treat as
  unknown rather than free or permitted.

Both are `SECURITY DEFINER` functions (migration `0018`); the runtime role can
execute them and still cannot `SELECT` a table.

## Published list prices

A provider's own model list sometimes publishes what a model costs (OpenRouter's
`pricing.prompt`/`pricing.completion`, USD per token). The inventory publisher
keeps that as a `providercost.ListPrice` — USD per million tokens, exact decimal
strings shifted from the per-token value without floating point — in the
deployment's `observed` block, and `GET /internal/v1/models` returns it per
deployment as `listPrices`.

That is deliberately not a cost and not a price. It is an observation of a
public catalogue, the same kind of fact as a context window: not what Kaana will
be invoiced for a request (that is a measurement from a versioned rate card or
the provider's receipt) and never what a customer pays (that is Oxy's). It is
carried so Oxy, the only caller of that signed operator route, can price models
automatically instead of keeping a hand-curated table. It is read only from a
catalogue whose documented unit and currency are a property of an
identity-bound endpoint, never inferred from a field called `pricing`; a
negative or unreadable value is left absent, never zeroed. It never enters an
inference stream event, a usage report, an error body or any contract shape.

## Rules a reviewer applies

- **`internal/providercost` is the only package that may hold an amount**, it is
  never the contract's money type, and `internal/contract` must not be able to
  reach it (asserted, not reviewed).
- **A cost never enters a stream event, a usage report, an error body or any
  customer-facing response.** It is an operator number; the customer's amount
  is Oxy's and always was. Its one way out of the process is the signed
  operator feed (`/internal/v1/provider-telemetry/*`, its own signature
  purpose), as a `providercost.OperatorAmount`.
- **Every attempt carries its own measurements or explicitly none.** Telemetry
  is never zero-filled for a row that predates it, and a replay that differs in
  any measured fact fails closed.
- **A rate-card version is immutable.** A changed price is a new
  `rateCardVersionId`; reusing one with different prices stops the process.
- **The one amount any response carries is a provider's PUBLISHED list price,
  on the signed `GET /internal/v1/models` to Oxy.** It is a
  `providercost.ListPrice` observation of a public catalogue, never Kaana's
  cost, never a customer amount, never in an inference response or contract
  shape, and never defaulted: unread means absent.
- **An unknown cost is never a zero cost.** A deployment with no rate card, or a
  measured unit nobody priced, says so and names what it could not price.
- **A failed failover attempt is off the customer's receipt and on Kaana's
  cost.** Do not merge the two: the customer never received that output and the
  provider will invoice for it regardless.

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
