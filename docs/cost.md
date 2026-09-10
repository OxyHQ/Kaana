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
read by their own package, and are keyed by deployment id. Amounts are integers
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

## Rules a reviewer applies

- **`internal/providercost` is the only package that may hold an amount**, it is
  never the contract's money type, and `internal/contract` must not be able to
  reach it (asserted, not reviewed).
- **A cost never enters a stream event, a usage report, an error body or a
  response of any kind.** It is an operator number; the customer's amount is
  Oxy's and always was.
- **An unknown cost is never a zero cost.** A deployment with no rate card, or a
  measured unit nobody priced, says so and names what it could not price.
- **A failed failover attempt is off the customer's receipt and on Kaana's
  cost.** Do not merge the two: the customer never received that output and the
  provider will invoice for it regardless.

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
