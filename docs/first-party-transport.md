# First-party transport rollout

Source implementation: 2026-10-23. Authority follows ADR 0004. This record does
not claim that the new SDK or outbound producer is deployed.

| Boundary | Source status | Release dependency |
| --- | --- | --- |
| Oxy signed instruction → Kaana | Compatible authority reader already released with contracts 8 | Keep external shapes and domain/billing checks |
| Own Node caller → Kaana HTTP/realtime | `kaanaForOxyService` shares Oxy core transport; source tested with core 7 candidate | Publish Oxy core 7, pin test dependency, SDK minor release, then caller adoption |
| Kaana Go API/publisher/control → Oxy | Existing workload producer still uses old exchange; separate migration pending | Oxy protocol-2 API must be live before producer deployment |
| Alia voice and Mention inference callers | Existing caller adoption pending SDK release | Preserve exact realtime resume credential and fixed attribution |

The SDK's tests compose real shared Oxy transport with HTTP and WebSocket
receivers: protocol-2 mint, token reuse, attribution, SSE, retries/idempotency,
resume, refusal, cancellation, redirects and disposal. Existing external SDK
contract tests remain in the same suite. Candidate tarballs are only local
validation; no local package path belongs in a committed manifest or lockfile.

Outbound Go migration must remove service API-key fallback, attest temporary
infrastructure credentials, request protocol 2 without scopes, and retain
callback/provider-validation audiences and cancellation. This is a separate
producer deployment: the compatible reader being live is insufficient. The
finite legacy workload bridge in Oxy cannot be retired until all producers and
queued work have been accounted for. Never reclassify external keys by branding.
