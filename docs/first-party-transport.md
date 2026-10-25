# First-party transport rollout

Source implementation: 2026-10-23. Authority follows ADR 0004. This record does
not claim that the new SDK or outbound producer is deployed.

| Boundary | Source status | Release dependency |
| --- | --- | --- |
| Oxy signed instruction → Kaana | Compatible authority reader already released with contracts 8 | Keep external shapes and domain/billing checks |
| Own Node caller → Kaana HTTP/realtime | `kaanaForOxyService` shares Oxy core transport; source tested with published core 7.0.0 | SDK minor release, then caller adoption |
| Kaana Go API/publisher/control → Oxy | Source `da32d3a` migrates all four commands to protocol 2 without key fallback | Oxy protocol-2 API is live and canary-verified; integrate and deploy this separate producer next |
| Alia voice and Mention inference callers | Caller source adopts the factory; final SDK pins and deployment remain pending | Preserve exact realtime resume credential and fixed attribution |

The SDK's tests compose real shared Oxy transport with HTTP and WebSocket
receivers: protocol-2 mint, token reuse, attribution, SSE, retries/idempotency,
resume, refusal, cancellation, redirects and disposal. Existing external SDK
contract tests remain in the same suite. Candidate tarballs are only local
validation; no local package path belongs in a committed manifest or lockfile.

Outbound Go migration must remove service API-key fallback, attest temporary
infrastructure credentials, request protocol 2 without scopes, and retain
callback/provider-validation audiences and cancellation. This is a separate
producer deployment: the compatible reader being live is insufficient. The
finite legacy workload bridge in Oxy cannot be retired until all producers,
queued work and scheduled/one-shot launch definitions have been accounted for.
An idle old task definition can launch an old producer later. Never reclassify
external keys by branding.

SDK 3.3.0 is prepared on top of the published contracts 8.0.1 security release.
Its test dependency now pins published Oxy core 7.0.0, with a registry lock and
no local overrides. The registry composition passes frozen installation, all
72 SDK tests, TypeScript and the SDK build; contracts 8.0.1 remains the exact
packed runtime dependency. Earlier candidate validation also covered the
contracts build, Biome and pack inspection. CI on the final SDK commit and the
Actions release remain required before consumer pins.

Oxy foundation main `4d8cc12a` is deployed; the coordinator verified protocol-2
scope-free mint, canonical receiver, wrong-audience rejection and the bounded
legacy transition on 2026-10-24. This satisfies the Go producer prerequisite,
not the producer deployment itself.

## Administrative launcher pin, 2026-10-25

The coordinator verified source `de3db4741ff648d9de8d28d124634a9b7553652e`
against ECR and all three running API/publisher containers:
`sha256:882eb76a904e60da364b352f3a0b6c98ce87dc50566d127e53587d50101dc2ec`.
The actual ECS services are `kaana` (two tasks, revision 150) and
`kaana-publisher` (one task, revision 151); their task families retain the
`oxy-kaana` prefixes. Both services were stable and API targets healthy.

The reviewed administrative launcher now pins this same image/source pair,
so a later pinned one-shot cannot restart the historical protocol-1 producer.
The operation-specific selection rules are unchanged: reviewed migration,
unpin and production-binding operations still select their triggering main
commit's image. Command allowlists, separate admin/migrator roles, migration
checks and deploy ordering remain intact. This source change runs no operation,
creates no service and does not claim a new credential-control deployment.
Existing schema and rollback requirements in `docs/operating.md` still apply.

Validation: the real workflow fragment tests preserve exact image selection and
migration commands; the launcher configuration changes only image/source fields.
Merge/deploy of this launcher update and the fleet-wide audit of scheduled and
one-shot definitions remain prerequisites to retiring the finite Oxy bridge.
