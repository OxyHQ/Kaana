# First-party transport rollout

Source implementation: 2026-10-23. Authority follows ADR 0004. This record does
not claim that the new SDK or outbound producer is deployed.

| Boundary | Source status | Release dependency |
| --- | --- | --- |
| Oxy signed instruction → Kaana | Compatible authority reader already released with contracts 8 | Keep external shapes and domain/billing checks |
| Own Node caller → Kaana HTTP/realtime | `kaanaForOxyService` shares Oxy core transport; source tested with core 7 candidate | Publish Oxy core 7, pin test dependency, SDK minor release, then caller adoption |
| Kaana Go API/publisher/control → Oxy | Source `da32d3a` migrates all four commands to protocol 2 without key fallback | Oxy protocol-2 API must be live before this separate producer deployment |
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

SDK 3.3.0 is prepared on top of the contracts 8.0.1 security release. Local
validation uses the composed Oxy core 7/contracts 6 candidate with patched
Zod 4.6.7. The committed development test dependency still needs its public
core 7 pin and regenerated registry lock after publication; this source and
candidate tarball are not the final registry-pinned CI gate.
The composed candidate passes all 72 SDK tests, TypeScript, contracts/SDK builds,
Biome and pack inspection; the tarball depends on contracts 8.0.1 exactly and
contains no local package references.
