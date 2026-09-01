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



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
