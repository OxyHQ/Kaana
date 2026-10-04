# Inert private publication plumbing

The publisher now obtains actual cards and the same-cycle SQL Decider before
constructing a private publication permit. Its source getter remains nil.
No provider credential, card file, attribution, catalogue, route, schema,
production configuration or source authorization is changed.

`make check` passed with Go 1.26.7, golangci-lint 2.13.2 and Bun 1.3.14. The
mutant that removed the factory's same-cycle guard failed on the stale-evidence
control. The source was restored byte for byte and the three affected package
suites passed with the race detector. PostgreSQL integration suites require a
separately owned database; the all-package run is not a production SQL proof.

The fixture exercises real Cards parsing, actual SQL-evidence reader boundary,
the Decider and internal snapshot construction. It uses synthetic discovery
and an in-memory evidence implementation; no authenticated provider request
or production publication occurred. The public builder and nil compiled
source remain closed.

The prospective shared identity is
`dep_openrouter_typesafe_jev_1_13_scoped_2026_10_04`, model reference
`typesafe/jev-1.13@2026-09-17`, dated upstream
`typesafe/jev-1.13-20260917`, provider card
`rc_xai_jev_reviewed_2026_10_04`, source version
`oxy-reviewed-prices/2026-10-04/xai-20260930+openrouter-jev-20260917`, and
Oxy price `jev_scoped_price_20261004_01`. Permit and idempotency IDs are
`jev-internal-synthetic-20261004-01`. These names are a review proposal, not
active authority. The exact principal, effective policy, signed catalogue,
actual card, evidence reference and deployment-ready finite expiry must be
reviewed together before replacing either source getter.

The existing OpenRouter key's custody was refreshed independently by the
root operator; no new account is proposed. Public endpoint price/ZDR evidence
is separate from internal-use approval, model privacy qualification and
resale rights. Customer resale and general Jev publication remain closed.

See `proof.json` for source and log hashes. Local records are retained beside
that proof.
