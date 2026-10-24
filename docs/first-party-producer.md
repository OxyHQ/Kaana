# First-party Go producer cutover

Source-only change dated 2026-10-23. No deployment or production database change
is implied. SDK transport and compatible inference readers are separate releases.

The API, publisher, credential-control and platform-credential-control commands
now use temporary AWS workload credentials exclusively. Their Oxy attestation
explicitly requests protocol 2 without scopes; external credential mint is not a
fallback. Existing API/control audience separation, callback selectors, queue
limits, retries, idempotency and provider-key custody remain in place.

1. Deploy Oxy's reader preparation, live binding/environment validation and
   protocol-2 workload mint/callback support first.
2. Confirm each exact production role binding is active, belongs to Kaana and
   carries the expected environment. Review read-only inventory before cutover.
3. Deploy this producer commit to API, publisher and credential-control workers.
   No database backfill or secret modification is part of this source change.
4. Verify real own grant admission/settlement, provider validation and bootstrap
   outcomes, activity reporting, and external customer refusals. Check callback
   failures and unreported validation outcomes; retain their evidence.
5. Remove the finite legacy bridge only after all ecosystem producers and queued
   work are accounted for. Rollback is the previous reviewed deployment while
   the Oxy reader remains compatible, never an automatic API-key fallback.

Tests cover protocol-2 wire shape, signed nonce/signature tampering, audience,
long-lived-key refusal, temporary role providers outside ECS, cancellation,
origin/redirect guards, callback precision, refresh and per-audience caching.
Registry SDK pins and caller adoption are tracked in the separate SDK branch.
