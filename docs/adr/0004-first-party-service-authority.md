# ADR 0004 — Oxy-owned services carry identity, not OAuth scopes

- Status: accepted
- Date: 2026-10-23
- Authority: the owner's request to remove internal connection permissions and API keys across Oxy products, while preserving external grants and each person's data boundary.

Oxy decides whether a caller is an attested first-party workload, a first-party
product session, or an external credential. Kaana receives that decision inside
the signed instruction; a request header or application name cannot confer it.

The signed envelope's `principal` and the grant's `principal` carry
`authority: 'first_party'` without a scope field for own workloads or product
sessions. The existing billing account, application, credential, environment,
request correlation and signed route limits remain authoritative. A first-party
grant cannot claim the `machine_credential` lane.

External principals carry `authority: 'external'` and their existing scope
arrays. An absent discriminator remains the historical external format; it
never implies first-party trust. This also keeps historical usage reports
readable. Empty or null scope fields on first-party instructions are refused,
not normalized into permission lists.

Kaana validates the signed instruction, its authority shape, recipient,
execution window, routes and settlement identity. The legacy external envelope
scope check does not apply to first-party execution. No synthetic wildcard or
filled scope array substitutes for that separation.

Deploy the compatible Kaana reader before Oxy emits the new first-party shape.
The contract descriptor and Go/Zod roundtrip fixtures cover both forms; canaries
must include ordinary generation, realtime, settlement and external refusal.

The SDK and outbound Go producer have distinct rollout stages; track them in
[the first-party transport record](../first-party-transport.md).
