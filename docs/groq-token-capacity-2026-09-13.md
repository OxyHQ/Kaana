# Groq token capacity refusal, 2026-09-13

Alia reference `chatcmpl-ab7ad115-e273-4bdb-899a-13dd28cd7a74` failed on
2026-09-12 at 23:29:45 UTC. Oxy/Kaana request
`5a5fccb0-71cb-4556-8b1c-cf26ce1f7237` ended on Groq with
`request_too_large`, no output and zero route switches.

A synthetic long-context reproduction through the published Oxy client returned
HTTP 413 from Groq, error type `tokens`, and a diagnostic identifying the
platform account's TPM limit: 8,000 available versus 11,305 requested.
This is different from an invalid payload or a model context-window limit.
The short human-session Alia control succeeded; the long one reproduced
`INVALID_REQUEST`. No original user prompt was exported or replayed.

The adapter now recognizes only Groq HTTP 413 with `type: tokens` and
`code: rate_limit_exceeded` as `rate_limited`. The existing executor may try
only subsequent Oxy-authorized routes. Other 413s, context-length refusals,
malformed requests and message-only lookalikes retain their terminal behavior.
The provider's retry header is preserved without inventing a delay.

The regression first failed on the old classification and passes with the fix.
A real HTTP adapter/executor test proves one primary attempt, exactly one
announced authorized replacement, no replacement without authorization and no
fallback for a genuine oversized payload.

## Serving backport

Until the schema-0013 cutover, production served an immutable backport of this
fix onto the then-serving source `df1fa39`, as `oxy-kaana:43`. The cutover
(2026-09-24, `docs/schema-0013-cutover-2026-09-24.md`) moved serving to images
built from main, which carries the fix, starting with `oxy-kaana:44`. The
backport's build workflow and its pinned patch were then deleted. Both remain in
git history, in commit `78c90c2` and earlier.
