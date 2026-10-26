# ADR 0005 — Resume realtime sessions without load-balancer cookies

- Status: accepted; deployment remains gated by the rollout evidence below.
- Date: 2026-10-26
- Authority: the owner's zero-cookie rule and approval of the private owner relay.

A realtime provider connection and its replay buffer live on one Kaana task.
Application and load-balancer cookies must not be prerequisites for reconnecting.
Replicating the provider connection or replaying admission would change execution
and settlement, so the session remains on its original owner.

An ephemeral PostgreSQL directory records only execution placement: request ID,
lane, private task IPv4, random process generation and expiring lease. Runtime
access uses narrow security-definer functions; it stores no user identity,
credential, audio or transcript. An old generation cannot renew or remove a new
owner's entry. The directory cannot create authority or extend a resume window.

A wrong-task resume makes one private connection to the recorded owner. The
owner proves its process generation before the relay sends any credential. The
owner then verifies the original signed internal resume or the public session's
original credential digest. It never performs another grant admission or opens
another provider connection. Replay, command deduplication and settlement remain
the original session's responsibility. Missing, expired or lost owners refuse
resume using the existing client contract.

Both endpoint address and peer ingress are bounded to the exact infrastructure
VPC and fixed serving port. ECS metadata supplies the current task address;
ALB-forwarded requests are refused before private upgrade. The private handshake
is a Kaana deployment implementation detail, not an Oxy or public SDK API.

Deployment is staged: additive migration and compatible server/infra first,
verification of all serving replicas, then the old-session drain, and only then
the separate removal of load-balancer cookies. See [the realtime rollout and
rollback procedure](../realtime.md#rollout-and-rollback). No caller scopes,
external credential limits, signed routes, pricing or settlement rules change.
