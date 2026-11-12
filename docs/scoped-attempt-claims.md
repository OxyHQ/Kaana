# Scoped operation attempt claims (retired)

> **Retired.** Contract set 7.0.0 removed the scoped permits this storage served. Kept only to explain migration 0020 and its table.


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

## Migration 0020 review procedure

Moved from `operating.md`.

Serving and publisher startup never migrate. Nothing claims an attempt any more
(contract set 7.0.0 removed the scoped and Private Auto lanes,
[scoped-attempt-claims.md](scoped-attempt-claims.md)); the migration and its
reviewed procedure stay because an applied migration is never rewritten. The credential-admin workflow selects the exact
current-main ECR digest for `migrate` only (beside the existing binding operations).
All rekey, deduplicate and ordinary metadata operations retain their fixed reviewed
image. Build the reviewed current-main image before dispatching migration; a missing
ECR source tag is a refusal, with no older-image fallback.

Run the supported workflow with operation `migrate`, phase `inspect` first. The
existing migrator task profile receives its existing verified-TLS database binding.
`migrate --reviewed-0020 --inspect-only` uses a read-only transaction and checks every
known migration checksum, rejects unknown ledger entries, and admits only pending
`0020` (or an already-applied no-op). It also verifies the recorder's definer/search
path and table/ledger consistency. The JSON report contains schema metadata only.

After reviewing that report, dispatch phase `apply`. The command independently
rechecks the same ledger, applies only `0020` inside one serializable transaction,
and compares every existing public relation/function owner and ACL plus role
memberships before/after. Any difference, or a non-owner grant on the new claim
table, rolls back. No role, task authority, secret binding or runtime grant is added.
Run phase `inspect` again to prove an empty pending set after the successful task.
The private source permit stays absent throughout this schema-only operation.
