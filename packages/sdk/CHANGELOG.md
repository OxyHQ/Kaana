# Changelog

## 3.12.0

Depend on contracts 8.9.0: `responses.create` takes `responseFormat.repair: true`
on a `json_object` or `json_schema` format, asking Kaana to repair nearly valid
JSON in a non-streaming answer (a code fence, surrounding prose, trailing commas,
closers a normal stop forgot). Off by default; refused with a streamed request;
never applied to an answer cut by the token limit. The type comes from the
contracts package; the SDK adds no method.

## 3.11.0

Depend on contracts 8.8.0: the client states contract set 8.8.0 (the optional
`cacheBreakpoint` on content parts and tool definitions). The SDK adds no method.

## 3.10.0

Depend on contracts 8.7.0: the client states contract set 8.7.0 (the Messages
endpoint accepts and ignores `cache_control`, `context_management`,
`thinking.display` and `tools[].defer_loading`). The SDK adds no method.

## 3.9.0

Depend on contracts 8.6.0: reasoning continuity (Kaana ADR 0009 decision 2).
`reasoning` takes `maxTokens` (a thinking budget; an alternative to `effort`)
and `exclude` beside `effort`, and an assistant message's content may carry
`{type: 'reasoning', text?, opaque?}` parts. A request that carries `reasoning`
(even `{}`) is sent each reasoning block's continuation token, and
`responses.stream(...).final` folds the blocks, in order, into `reasoning`
parts ahead of the answer: send the assistant turn back unmodified, tool calls
included, to continue a thinking model across a tool turn. A request without
`reasoning`, or with `exclude: true`, folds to the same response as before. The
token is sealed, not secret: return it exactly as received.

## 3.8.0

Depend on contracts 8.5.0: the client states contract set 8.5.0 and its request
types gain the optional `provider` routing preference and the prompt-cache key.

## 3.7.0

Depend on contracts 8.4.0: the client states contract set 8.4.0, which declares
the usage unit `cache_write_input_tokens` and the optional
`capabilities.unreported`. The SDK adds no method and the request shapes it
sends are unchanged.

## 3.6.0

Depend on contracts 8.3.0: the client states contract set 8.3.0, which adds the
Anthropic-Messages-compatible `POST /v1/messages`. The SDK adds no method for
it: that endpoint is for Anthropic's own SDK and Claude Code, pointed at
`https://kaana.ai`. The request shapes the SDK sends are unchanged.

## 3.5.0

Depend on contracts 8.2.0: the client states contract set 8.2.0, whose image
rules (Kaana ADR 0007) Kaana applies to an `image` part: inline base64 only,
PNG, JPEG, WebP or GIF, at most 3,750,000 bytes each and 20 a request. The
request shapes are unchanged.

## 3.4.0

Depend on contracts 8.1.0: a realtime session parses `transcript.interim`, the
provisional text a `transcription` session sends when opened with
`inputAudioTranscription.interimResults: true` (Kaana ADR 0006), and
`realtime.connect` refuses that flag on any other kind before dialling. The
testing fake emits it like any other event. Nothing else changes.

## 3.3.0

Add `kaanaForOxyService` for own HTTP and realtime callers. The supplied shared
Oxy workload transport manages authentication, destination binding and refresh;
the SDK preserves its retries, streaming and realtime resume credential.
Inference attribution is immutable and remains app-paid accounting. Existing
external credential clients are unchanged. Requires contracts 8.0.1 and its
patched Zod dependency; the factory host needs Oxy core 7 or newer.

## 3.2.1

Depend on contracts 8.0.1, which requires patched Zod 4.6.7 or newer within
major 4. The SDK public API and wire behavior are unchanged; the contract
retains its UTF-16 string bounds and UTC timestamp grammar.
