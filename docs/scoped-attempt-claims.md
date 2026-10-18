# Scoped operation attempt claims (retired)

Migration 0020 added durable at-most-one claim storage
(`scoped_provider_attempt_claims`) and an `attempt_claimed` / `local_admission`
branch of the credential-attempt recorder, for the one-off scoped and Private
Auto decisions permits. Contract set 7.0.0 removed those permits: decisions are
an ordinary grant on `https://kaana.ai/v1/decisions` (decisions.md), and no
code path claims an attempt.

The table, the recorder branch and the reviewed migration procedure
(`operating.md`, "Reviewed scoped claim migration 0020") remain, because an
applied migration is never rewritten. Dropping them is a separate reviewed
migration; until then the table is inert and receives no rows.
