# Scoped operation attempt claim (inactive)

Migration 0020 adds durable at-most-one claim storage. It replaces the existing
credential-attempt recorder body with an explicit `attempt_claimed` /
`local_admission` branch, preserving its signature, owner and EXECUTE ACL. It
adds no role, scope, table read permission or GRANT. Ordinary provider outcomes
follow the byte-for-byte unchanged previous branch. Claim rows never masquerade
as provider outcome or cost evidence.

`Postgres.ClaimScopedAttempt` returns true only on the first committed insertion
for one stable permit ID (independent of HTTP request IDs). Duplicates, changed bindings, expired claims, database
errors and cancellation must prevent sending. A claim is never released; an
uncertain send or process restart cannot authorize a second attempt. The claim
locks and checks the active exact deployment/key binding for the claim statement and permits at most a
five-minute operation window. It is not request authorization or eligibility.

This change does not attach claims to execution, publish a deployment, approve
an adapter or activate any provider. The next integration must validate the
signed scoped audience and ordinary Oxy policy/funding/catalogue attestation,
then claim before the first upstream send with an exact one-key pool. Ordinary
requests and production constructors remain closed for decisions.

Owned PostgreSQL integration covers concurrent 16-way admission with one winner,
replay, wrong binding, expiration, absence of fabricated telemetry, runtime
SELECT/INSERT denial, before/after function owner/ACL/definer/search_path equality,
and existing same-route outcome recording compatibility. No real key or prompt
is used. Execution integration and independent review remain release gates.

Binding eligibility is a claim-time observation. The claim does not lock a
credential for the entire network exchange or grant immunity from later
revocation. Signed audience and ordinary eligibility must be checked before
claiming; claim success is only the single-use condition.
