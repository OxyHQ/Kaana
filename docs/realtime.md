# Realtime sessions

A realtime session is the one inference family that is not a request: a signed
session request opens a long-lived WebSocket conversation, the client keeps
sending commands, and Kaana keeps sending events, for up to an hour. It is one
request for attribution, metering and settlement, and it settles exactly once.
Contract: `@oxy.so/contracts` 4.5.0 (inference set 3.3.0: 3.2.0's session
family plus the `session_milliseconds` unit), `inference/realtime.ts`;
Go: `internal/contract/realtime.go`, `realtime_wire.go`. Issue: OxyHQ/Kaana#90.

| Piece | Where |
|---|---|
| The edge↔Kaana wire, below | `internal/realtime` (`GET /internal/v1/realtime`) |
| Routing a session open, settling it | `kaana.Executor.OpenSession`, `SettleSession` |
| The provider-side session interface | `provider.RealtimeAdapter`, `provider.RealtimeUpstream` |
| OpenAI Realtime (GA) | `internal/provider/openairealtime`, slug `openai-realtime` |
| xAI Voice Agent (the same adapter, xAI's dialect) | `internal/provider/openairealtime` (`dialect.go`, `meter.go`), slug `xai-realtime` |
| The real-wire fake upstreams | `internal/provider/openairealtime/openairealtimetest` (`New`, `NewXAI`) |

## The wire between the edge and Kaana

This is fixed: the Oxy edge is built against it. It is reproduced exactly.

> **Kaana realtime session wire (edge <-> Kaana), binding for both sides**
>
> Contract: @oxy.so/contracts 4.4.0 (inference set 3.2.0), module `inference/realtime.ts`
> (realtimeSessionRequestSchema, realtimeClientCommandSchema, realtimeServerEventSchema).
> Go types: Kaana `internal/contract/realtime.go`, `realtime_wire.go` (branch feat/90-contracts-4.4).
>
> 1. Endpoint: `GET /internal/v1/realtime` on the Kaana runtime (same origin as
>    `POST /internal/v1/inference`), WebSocket upgrade (RFC 6455). No subprotocol.
> 2. Authentication: the upgrade request carries the SAME three edge signature headers the
>    inference envelope carries (`edgeauth.HeaderKeyID`, `HeaderTimestamp`, `HeaderSignature`,
>    i.e. X-Oxy-Kaana-Key-Id / -Timestamp / -Signature — read exact names from
>    Kaana internal/edgeauth/edgeauth.go), computed with the INFERENCE domain
>    (`edgeauth.SigningInput`, domain "oxy-kaana-envelope:v1") over the EXACT BYTES of the
>    first text frame the edge will send on that connection. Kaana completes the upgrade,
>    reads the first frame (bounded, 64 KiB, within 10 s), and verifies the signature over
>    those bytes; on failure it closes with WebSocket close code 1008 and sends nothing else.
>    Skew bound is the normal edge skew (5 min).
> 3. First frame: either a `realtimeSessionRequestSchema` JSON (opens a session) or a
>    `session.resume` command JSON (resumes one). Anything else: close 1008.
> 4. Framing: every frame is one JSON TEXT message. Client->Kaana: commands. Kaana->client:
>    server events. A binary frame from the edge closes the session (close 1003,
>    after `error` fatal + `session.closed` if a session was open).
> 5. Every event carries `sequence`, from 0, monotonic per session. `command.accepted` is sent
>    for every command BEFORE it is applied; a repeated `commandId` gets
>    `command.accepted{duplicate:true}` and is not applied again. Kaana never replays a
>    command upstream.
> 6. Settlement: after `session.closed` Kaana sends EXACTLY ONE more text frame whose JSON is a
>    `normalizedUsageReportSchema` for `attribution.requestId` (outcome completed / failed /
>    cancelled as usual; units = the session's totals; deploymentId = the session's
>    deployment; generationId as for one-shot), then closes with code 1000. If that frame is
>    lost, the edge recovers usage by requestId exactly as for one-shot requests. A session
>    that never opened on any route closes with `session.closed{reason:no_route_available}`
>    with empty units, and its usage report has outcome `failed` and no units.
> 7. Resume: sessions are held in the memory of the Kaana task that opened them. A
>    `session.resume` on a connection that reaches a task that does not hold the session (or
>    after `resumeWindowMs`, or with an `afterSequence` no longer buffered) is answered with
>    `error{fatal:true, error.code:"invalid_request"}` and close 1008; the original session
>    then closes itself with `resume_expired` when its window elapses and delivers its report
>    only to a connection that is attached at that time (otherwise the edge recovers by
>    requestId). `session.created.resumeWindowMs` states the window (Kaana default 30000).
> 8. Limits: Kaana enforces `limits` exactly: maxDurationMs (close `max_duration`),
>    idleTimeoutMs = no client command received for that long (close `idle_timeout`),
>    input/output audio bytes counted on DECODED audio (close `limit_exceeded`),
>    maxResponses counted on `response.created` (close `limit_exceeded`).
> 9. Kaana closes the session with `server_shutdown` on SIGTERM drain.
> 10. Concurrency: one attached connection per session at a time; a resume detaches the old one.

### What Kaana does where the wire leaves a choice

- **A frame that is JSON but not a valid session request** (wrong
  `schemaVersion`, a failed published refinement, an unknown request shape) is
  "anything else": close 1008, reason `invalid realtime session request`,
  nothing routed. A `requestId` that already names a live session on this task
  is refused the same way. Unknown fields are tolerated, as for every inbound
  envelope (`contract.md`).
- **A session request that is valid but cannot be served** — no
  `inference:invoke`, an inventory disagreement, a customer credential, a
  refused kind, every route failing — is a session that never opened:
  `error{fatal:true}` naming why, `session.closed{no_route_available}`, the
  failed report, close 1000. The edge learns why, which a 1008 could not say.
- **A resume refusal** carries `sequence: 0` and the resume's `commandId`. It is
  sent on a connection that never attached, so it is outside the session's
  sequence and consumes no number.
- **`session.resume` itself** is answered by `session.resumed`, which carries
  its `commandId`, not by `command.accepted`: it is the connection's first
  frame, and the replay must precede any new sequence number. A
  `session.resume` sent later on an attached connection is refused with a
  non-fatal `invalid_request`.
- **A resume replaces the attached connection**, which is closed with 1001
  (going away), not 1000: 1000 is the settled end of the session.
- **A command that does not decode** is refused with a non-fatal
  `invalid_request` carrying its `commandId` when one could be read; the session
  continues. It does not reset the idle timer; any decoded command does,
  duplicates included.
- **A command that would pass a limit is not acknowledged.** An acknowledgement
  means the command was applied, and one that would pass a signed ceiling is
  not applied at all: `error{fatal:true}` with its `commandId`, then
  `session.closed{limit_exceeded}`.
- **A command the provider cannot express** (`provider.ErrUnsupported`) was
  acknowledged, then is refused with a non-fatal error naming the field; nothing
  reached the provider. A command whose write to the provider FAILED ends the
  session (`upstream_error`): whether it applied is unknowable, and it is never
  resent.
- **Quiet connections are pinged** every 20 s, so a load balancer's idle timeout
  (60 s by default on an AWS ALB) never drops a session that is only listening.
- **A binary frame** ends the session with reason `client_closed` (the client
  broke the framing), the fatal error, the usage report, and close 1003.

## Opening: routing a session

`kaana.Executor.OpenSession` is routing, and it is the executor's for the same
reason routing a request is. The signed `authorizedRoutes` are resolved by the
same function a request uses (`resolveAuthorizedRoutes`), so a session and a
request cannot disagree about what a route means. Then, in signed order:

1. the deployment's breaker admits it, or it is skipped;
2. `Registry.ResolveRealtimeExecution` resolves the session adapter and the
   deployment's exact platform key by the three rules in `key-pools.md`. A
   deployment served by a REQUEST adapter is refused (`unsupported_modality`,
   `provider.ErrNotASessionAdapter`) — a text adapter is never handed a session.
   No adapter, or no exact binding, is a configuration gap and is skipped;
3. the adapter's `Open` dials and configures the session through
   `provider.WalkAttempts`, which is `Walk`'s credential discipline: a refused
   key is retired, an exhausted account is retired, a throttle retires nothing,
   and every attempt is recorded as evidence against its exact key;
4. an attributable failure (`provider.DeploymentAttributable`) moves to the
   next signed route; a request fault, a refusal and a cancellation end the
   open.

A session **opens** when the provider confirmed its configuration — not when
the socket connected. Once `session.created` is sent the deployment is fixed
for the session's life. There is no mid-session failover: the conversation is
upstream, and a second provider does not have it.

`routeSwitches` on the report counts the routes that were attempted and failed
before the one that served (or, for a session that never opened, before the
last one tried). There is no `route_switch` event in the realtime family, so
`session.created` naming the deployment is the whole announcement.

**A customer provider credential is refused** (`invalid_request`,
`authorizedRoutes[i].customerProviderCredential`). BYOK custody decrypts a key
for one upstream call and destroys it with the call
(`customer-provider-credentials.md`). A session would hold the plaintext for up
to an hour and needs it for exact-match redaction all that time. That is a
custody decision nobody has made, so it is refused rather than taken here.

## The session

`internal/realtime` owns the session between open and settlement. One
goroutine owns all of its state, so one goroutine numbers every event and no
two can share a sequence.

| Concern | Rule |
|---|---|
| Sequence | From 0, one per event, assigned when the event is emitted |
| Replay buffer | The encoded bytes of the most recent events, bounded at 8192 events and 16 MiB, oldest forgotten first |
| Commands | `command.accepted` before applying; a seen `commandId` answered `duplicate:true` and never applied again; nothing ever resent upstream |
| Resume window | 30 s (`session.created.resumeWindowMs`), from the moment the connection is lost |
| Input audio | Decoded bytes of `input_audio.append` and `input_audio` content parts, checked before acknowledging |
| Output audio | Decoded bytes of `output_audio.delta`; the delta that would pass the ceiling is not delivered |
| Responses | Counted on `response.created`; the one past the ceiling is not delivered, and `response.create` at the ceiling is refused |
| Idle | No decoded client command for `idleTimeoutMs` (runs while detached too) |
| Duration | `maxDurationMs` from `session.created` (`expiresAt`) |
| Shutdown | SIGTERM closes every session with `server_shutdown` and settles it inside the 30 s drain; new upgrades get 503 |

| Close reason | Fatal error before it | Report outcome |
|---|---|---|
| `client_closed` (`session.close`) | none | completed, or cancelled if nothing was measured |
| `client_closed` (binary frame) | `invalid_request` | as above; the attempt is recorded as cancelled |
| `limit_exceeded` | `request_too_large` (input audio) or `output_limit_exceeded` | completed, or cancelled if nothing was measured |
| `max_duration`, `idle_timeout`, `server_shutdown`, `upstream_closed` | none | completed, or cancelled if nothing was measured |
| `resume_expired` | none | cancelled |
| `upstream_error` | the provider's classified failure | partial with units, failed without |
| `no_route_available` | why nothing opened | failed, no units |

A completed report must carry a unit (`UsageReport.Validate`), which is why a
session that measured nothing and ended normally settles as cancelled.

## Settlement: exactly once

`finish` runs once per session, from the one goroutine:

1. the fatal error, if the session ends on one;
2. `session.closed` with the reason, the deployment (absent when nothing
   opened) and the session's units: the sum of every unit the provider
   reported, each unit once — or, for a provider billed by what Kaana
   measures (`provider.RealtimeMeter`, xAI), that measurement, read once here
   and labelled `oxy_measured` on `session.closed` and the report;
3. the provider session is closed;
4. `SettleSession` builds the usage report and prices and records the operator
   cost: one `provider_cost_events` row per attempt — each failed open with its
   key, outcome and failure code, and the served session with its key, its
   units, `latency` = the session's whole life (monotonic clock from the open
   attempt's start), `timeToFirstOutput` = the first audio, text, transcript or
   tool-call event, and its outcome (`failed` with the code for
   `upstream_error`, `cancelled` for `resume_expired` or a binary frame,
   otherwise `succeeded`);
5. the usage report frame to the connection attached at that moment, if any,
   then the close (1000; 1003 after a binary frame).

A reconnect never settles anything. The session is removed from the task once
it has settled, so a resume after that is refused and the edge recovers the
report by `requestId`.

Credential evidence follows the one-shot rules exactly: verdicts come from the
OPEN, before anything reached the client. A failure arriving mid-session,
after `session.created`, retires nothing — the request was committed to the key
that opened it (`key-pools.md`, "A rotation happens only before the response
body is read").

## The provider side: `provider.RealtimeAdapter`

```go
type RealtimeAdapter interface {
	Provider() contract.ProviderSlug
	RealtimeSessionKinds() []contract.RealtimeSessionKind
	Open(ctx, RealtimeOpenRequest, *KeyPool) (RealtimeUpstream, RealtimeOpened, error)
	Health(ctx) Health
}

type RealtimeUpstream interface {
	Send(ctx, contract.RealtimeCommand) error        // once; ErrUnsupported before anything is written
	Next(ctx) (RealtimeUpstreamEvent, error)         // normalized event + units; ErrRealtimeUpstreamClosed
	Close() error
}
```

It is not `provider.Adapter`, and a slug is exactly one of the two: the registry
refuses a type that implements both, one that implements neither, and a session
adapter that declares no kind (`TestASlugIsExactlyOneKindOfAdapter`, with its
positive controls). An adapter declares its kinds from
`providerconfig.RealtimeSessionKinds`, the session counterpart of
`ExecutableAPIFormats`, which the publisher reads too.

What a session adapter does not do is Adapter's list: it allocates no id,
numbers nothing, decides no terminality beyond reporting that its upstream
ended, and never decides what a failure means for the key.

## OpenAI Realtime (GA): `openai-realtime`

OpenAI's own origin under a third slug, for the reason `openai-audio` is one:
a slug resolves to exactly one adapter, so a Realtime model published under
`openai-realtime` can only be opened by this adapter, and a request can never
reach it.

| | |
|---|---|
| Protocol | `openai_realtime` (only under the `openai-realtime` slug) |
| Configured root | `https://api.openai.com/v1`, locked; the publisher reads the account's `GET /v1/models` there |
| Session endpoint | `wss://api.openai.com/v1/realtime?model=<signed upstream id>`, fixed in `providerconfig` |
| Authentication | `Authorization: Bearer <key>` on the handshake; no `OpenAI-Beta` header (GA) |
| Session kinds | `conversation` only |

**Opening.** Dial (a non-101 handshake is classified from its status and
OpenAI's error body), wait for `session.created`, send one `session.update`
with the whole configuration (`event_id: kaana-session-open`), and wait for
`session.updated`. An `error` in between is OpenAI's answer about this session
on this key and is classified like a refused request. The open is bounded at
20 s per stage.

### Configuration

| Contract | OpenAI GA `session.update` |
|---|---|
| (always) | `session.type: "realtime"` |
| `instructions` | `instructions` |
| `outputModalities` `["audio"]` / `["text"]` | `output_modalities` `["audio"]` / `["text"]`; `["text","audio"]` is refused (OpenAI produces one or the other) |
| `inputAudioFormat` `pcm16_24khz` / `g711_ulaw` / `g711_alaw` | `audio.input.format` `{type:"audio/pcm",rate:24000}` / `{type:"audio/pcmu"}` / `{type:"audio/pcma"}` |
| `outputAudioFormat`, `voice` | `audio.output.format`, `audio.output.voice` |
| `turnDetection` `none` | `audio.input.turn_detection: null` |
| `turnDetection` `server_vad` | `{type:"server_vad", threshold?, prefix_padding_ms?, silence_duration_ms?, create_response, interrupt_response}` |
| `turnDetection` `semantic_vad` | `{type:"semantic_vad", eagerness, create_response, interrupt_response}` |
| `tools` (function) | `tools: [{type:"function", name, description?, parameters}]`; `strict` is refused (GA has none) |
| `toolChoice` | `tool_choice`: the mode string, or `{type:"function", name}` |
| `maxOutputTokens` | `max_output_tokens`; above 4096 is refused (GA schema: 1..4096 or `"inf"`) |
| `temperature` | refused: absent from the GA session and response schemas |
| `inputAudioTranscription` | refused (see findings) |

### Commands

| Contract command | OpenAI client event (`event_id` = `commandId`) |
|---|---|
| `session.update` | `session.update` (the same mapping; confirmed by `session.updated` in order, or refused by an `error` naming the event) |
| `conversation.item.create` | `conversation.item.create {previous_item_id?, item}`; `input_audio` parts carry `audio` (+ `transcript`) in the session's input format, so a part in another format is refused; assistant `output_audio` is refused (a client cannot create it) |
| `conversation.item.delete` | `conversation.item.delete {item_id}` |
| `conversation.item.truncate` | `conversation.item.truncate {item_id, content_index, audio_end_ms}` |
| `input_audio.append` / `.commit` / `.clear` | `input_audio_buffer.append {audio}` / `.commit` / `.clear` |
| `response.create` | `response.create {response: {instructions?, output_modalities?, max_output_tokens?, tool_choice?}}` |
| `response.cancel` | `response.cancel {response_id?}` |
| `session.close`, `session.resume` | never sent upstream; the session answers them |

### Events

| OpenAI server event | Contract event |
|---|---|
| `session.updated` (after open) | `session.updated` with the effective configuration, `commandId` of the update it confirms |
| `conversation.item.added` / `.done` | `conversation.item.added` / `.done`; an item the contract cannot carry faithfully (a message with no content yet, an MCP item, an image part) is dropped |
| `conversation.item.deleted` / `.truncated` | `conversation.item.deleted` / `.truncated` |
| `input_audio_buffer.speech_started` / `.speech_stopped` | `input_audio.speech_started` / `.speech_stopped` |
| `input_audio_buffer.committed` / `.cleared` | `input_audio.committed` / `.cleared` |
| `response.created` | `response.created` |
| `response.output_audio.delta` | `output_audio.delta`, split into frames of at most `provider.MaxAudioChunkBytes` (48 KiB) decoded bytes, in order |
| `response.output_audio.done` | `output_audio.done` |
| `response.output_audio_transcript.delta` / `.done` | `transcript.delta` / `.done`, `source: output_audio` |
| `response.output_text.delta` | `text.delta` |
| `response.function_call_arguments.delta` / `.done` | `tool_call`: the name (from `response.output_item.added`) on the first increment, argument text as it streams, and the arguments on completion only if none were streamed |
| `response.done` | `response.done` with status, finish reason and the units below |
| `error` | `error{fatal:false}`, `commandId` from `error.event_id`; the session stays open, as OpenAI states most errors leave it |
| `session.created` (after open), `conversation.created`, `rate_limits.updated`, `response.output_item.*`, `response.content_part.*`, `response.output_text.done`, `input_audio_buffer.timeout_triggered`, `output_audio_buffer.*` (WebRTC/SIP only), MCP events | dropped: the contract has no event for them |

The connection ending: a close 1000 is `session.closed{upstream_closed}`; any
other end, or an end after OpenAI reported a failure about the session itself,
is `upstream_error` with that failure.

### Usage

`response.done.usage` nests the other way round from the contract's
partition: `input_tokens` INCLUDES cached and audio tokens,
`input_token_details.audio_tokens` INCLUDES cached audio, and `output_tokens`
INCLUDES audio output. Every unit is therefore a subtraction:

```text
cached_audio_input_tokens = input_token_details.cached_tokens_details.audio_tokens
audio_input_tokens        = input_token_details.audio_tokens - cached_audio_input_tokens
cached_input_tokens       = input_token_details.cached_tokens - cached_audio_input_tokens
input_tokens              = input_tokens - input_token_details.cached_tokens - audio_input_tokens
audio_output_tokens       = output_token_details.audio_tokens
output_tokens             = output_tokens - audio_output_tokens
```

A negative result, or a `total_tokens` that is not the sum, is a report this
adapter cannot have read correctly: the session ends (`upstream_error`) rather
than settling a guess. A response without usage reports none (`units: []`),
never zeros. `TestUsagePartitionsCachedAudio` pins the arithmetic with cached
text AND cached audio, the case where subtracting only one double-charges the
other.

### Errors

Classified by OpenAI's own type and code, never by prose
(https://developers.openai.com/api/docs/guides/error-codes). Handshake: 401/403
→ `provider_credential_invalid` (retires the key); 402, or 429 with
`insufficient_quota` / `credit_balance_exhausted` / the spend- and usage-limit
codes → `provider_billing_refused` (retires the key); 429 otherwise →
`rate_limited` with `Retry-After`; 404 → `model_not_found`; 408/504 →
timeout; 503 → overloaded; 5xx → `provider_error`; other → `invalid_request`.
In-band `error` events use the same vocabulary by type/code (`invalid_api_key`,
`insufficient_quota`, `rate_limit_error`, `server_error`,
`invalid_request_error`). Every message is stripped of this key by exact match
before `contract.SafeErrorText`.

### Sources

Reviewed against OpenAI's documentation on 2026-09-30:

- [Realtime over WebSocket](https://developers.openai.com/api/docs/guides/voice-websockets),
  [conversations](https://developers.openai.com/api/docs/guides/realtime-conversations),
  [VAD](https://developers.openai.com/api/docs/guides/realtime-vad),
  [latency and cost](https://developers.openai.com/api/docs/guides/voice-latency-cost)
- [Client events](https://developers.openai.com/api/reference/resources/realtime/client-events),
  [server events](https://developers.openai.com/api/reference/resources/realtime/server-events)
- [Translation](https://developers.openai.com/api/docs/guides/realtime-translation),
  [transcription](https://developers.openai.com/api/docs/guides/realtime-transcription)
- Models: [gpt-realtime-2.1](https://developers.openai.com/api/docs/models/gpt-realtime-2.1),
  [gpt-realtime-2.1-mini](https://developers.openai.com/api/docs/models/gpt-realtime-2.1-mini),
  [gpt-realtime-2](https://developers.openai.com/api/docs/models/gpt-realtime-2),
  [gpt-realtime-translate](https://developers.openai.com/api/docs/models/gpt-realtime-translate),
  [gpt-live-transcribe](https://developers.openai.com/api/docs/models/gpt-live-transcribe),
  [gpt-realtime-whisper](https://developers.openai.com/api/docs/models/gpt-realtime-whisper)

No live OpenAI call has been made from this repository. The fake speaks the
wire as the reference documents it; every "UNVERIFIED" below is a place where
the documentation does not say and the fake therefore cannot know.

## xAI Voice Agent: `xai-realtime`

xAI's Voice Agent (speech-to-speech) API is served by the same adapter in
xAI's dialect (`internal/provider/openairealtime/dialect.go`), under its own
slug for the reason `openai-realtime` is one: `xai` is already the
OpenAI-compatible request adapter, and a slug resolves to exactly one adapter.
xAI documents the API as OpenAI-Realtime-compatible and lists the differences;
the handshake, the configure-then-confirm open, the command and event
vocabulary, the item model, tool-call accumulation and audio framing are the
shared code, and every documented difference is a dialect field.

| | |
|---|---|
| Protocol | `xai_realtime` (only under the `xai-realtime` slug) |
| Configured root | `https://api.x.ai/v1`, locked; the publisher reads the account's `GET /v1/models` there and, for the voice models that list omits, opens a read-only session (`xai_models_and_realtime_sessions`, "Publishing") |
| Session endpoint | `wss://api.x.ai/v1/realtime?model=<signed upstream id>`, fixed in `providerconfig` |
| Authentication | `Authorization: Bearer <key>` on the handshake |
| Session kinds | `conversation` only (xAI documents no other) |
| Model | `grok-voice-think-fast-2.0`, the pinned flagship; `grok-voice-latest` is xAI's alias for it and is not attributed |
| Turn detection | `none` (push-to-talk, billed by audio) or `server_vad` (billed by session wall clock) — see "Metering" |
| Session limit | xAI's own: 120 minutes, 10 concurrent sessions per team at tier 0 |

### What differs from OpenAI's dialect

| | OpenAI GA | xAI |
|---|---|---|
| `session.type` | `"realtime"` | absent from xAI's schema; not sent |
| Voice | `audio.output.voice` | `session.voice` |
| Turn detection | `audio.input.turn_detection`; none is `null` | `session.turn_detection`; none is `{"type": null}` |
| `server_vad` | `create_response`, `interrupt_response` sent | only `threshold` (0.1–0.9), `prefix_padding_ms` and `silence_duration_ms` (0–10000); `createResponse` and `interruptResponse` must be `true` (xAI's VAD always answers and is interruptible; it has neither field); a text-only session is refused (xAI creates the responses, with no session modalities field) |
| `semantic_vad` | served | refused: xAI's schema names only `server_vad` or null |
| Output modalities | `session.output_modalities`, one per response | no session field: the session's modalities are sent as `response.modalities` on every `response.create` the client does not override (every response of a push-to-talk session is created by one), and `["text","audio"]` together is accepted; a `server_vad` response xAI creates itself carries xAI's own modalities |
| `tool_choice`, `max_output_tokens`, `temperature` | the first two served | none documented: refused naming the field |
| Function tools | flat `{type, name, description, parameters}` | the guide's examples are flat; the machine-readable schema nests under `function` (UNVERIFIED which xAI enforces; flat is sent) |
| Assistant content parts | `output_text` / `output_audio` | `text` / `audio` (read back as the contract's output parts for the assistant, input parts otherwise) |
| Event aliases | — | `response.text.delta` = `response.output_text.delta` ("Functionally identical... handle both"); `response.audio.delta` = `response.output_audio.delta` |
| `conversation.item.done` | sent | not emitted by xAI; the contract event simply does not occur |
| In-band errors | OpenAI's type/code vocabulary | `invalid_request_error` / `invalid_event` refuse one event (session continues); `internal_error` is xAI's failure; `timeout` / `max_duration` end the session (`provider_timeout`) |
| Handshake refusal body | `{"error": {type, code, message}}` | `{"code": "<status text>", "error": "<message>"}` (probed, see below) |
| Usage | `response.done.usage` tokens, billed | tokens reported but NOT billed: never settled; the session is metered instead |

Input transcription (`grok-transcribe`) is refused for the reason OpenAI's is,
and because xAI does not document what enabling it costs. xAI's extensions
(server-side `web_search`/`x_search`/`file_search`/`mcp` tools, `force_message`,
resumption, `replace`, binary transport, `idle_timeout_ms`,
`reasoning.effort`) have no contract field and are never sent; `reasoning.effort`
therefore stays at xAI's default (`high`).

### Metering: what xAI bills, in contract units

xAI's pricing (https://docs.x.ai/developers/models/speech-to-speech,
https://docs.x.ai/developers/pricing, read 2026-09-30):

> Audio $0.08 / minute or $4.80 / hour
> Text Input $0.004 per conversation.item.create event
> Sessions using the default `server_vad` turn detection are billed for session
> duration. Push-to-talk sessions are billed only for audio sent and received.
> Every `conversation.item.create` event you send from the client is billed at
> $0.004, with two exceptions: `function_call_output` items (server-requested
> tool results) are not billed. Items whose content is `input_audio` or `audio`
> are billed by the audio meter instead.
> `response.create` is not billed as a text input.

`response.done.usage` carries only `input_tokens`/`output_tokens`/`total_tokens`,
which xAI does not bill for voice, and no duration. So the adapter meters the
session itself (`meter.go`, `provider.RealtimeMeter`), in one of two modes fixed
by the turn detection the session opened with:

| xAI charge | Mode | Contract unit | Measured as |
|---|---|---|---|
| audio sent | push-to-talk | `audio_input_milliseconds` | decoded bytes of every `input_audio_buffer.append` and `input_audio` item part Kaana wrote upstream, at the input format's rate (PCM16 24 kHz = 48 bytes/ms, G.711 = 8 bytes/ms) |
| audio received | push-to-talk | `audio_output_milliseconds` | decoded bytes of every output audio delta xAI sent, at the output format's rate, whether or not the customer received it |
| session duration | `server_vad` | `session_milliseconds` | wall clock (monotonic) from the accepted handshake of the attempt that opened to the first of: xAI ending the connection, or Kaana closing it at settlement |
| text input event | both | `requests` | each written `conversation.item.create` that is not a `function_call_output` and carries no audio |

A `server_vad` session reports **no** audio units: its clock already bills that
span (xAI: "Items whose content is input_audio or audio are billed by the audio
meter instead", and under `server_vad` the audio meter is the session clock),
and the contract forbids a per-audio reading beside `session_milliseconds` for
the same span. A push-to-talk session reports no clock. A `session.update`
that would switch between the two modes is refused before anything is written
(`config.turnDetection`): xAI documents each mode's billing, not a session that
changes mode, so open a new session. Retuning `server_vad` itself is served.

Milliseconds are rounded up once over the session total. The units are read
once when the session settles, added to its totals and labelled `oxy_measured`
on `session.closed`, the usage report and the operator record;
`response.done` carries no units for xAI. An item carrying both text and audio
is refused (`item.content`): xAI does not say which meter bills it. A command
that is refused or never written is not measured.

**Why `session_milliseconds` and not the audio units.** A `server_vad` session
is billed for its whole duration — wall clock, speech or silence — which is
neither audio sent nor audio received. Reporting that clock under
`audio_input_milliseconds` would misdescribe the charge, and reporting both
would bill the same span twice, so contract set 3.3.0 added
`session_milliseconds`: "wall-clock milliseconds a realtime session was open with
the upstream provider; reported only by a provider that bills session time, and
never together with a per-audio reading of the same span." The clock starts at
the accepted handshake because that is when xAI's session exists; Kaana's open
is bounded at 20 s per stage, which is why Oxy holds `maxDurationMs + 60 s` of
it (UNVERIFIED: the exact instant xAI starts and stops its own billing clock; the
first signed canary should compare a session's `session_milliseconds` with xAI's
usage for it).

A rate card prices the four units per deployment in 10⁻¹² USD
(`providercost.Scale`): `audio_input_milliseconds`, `audio_output_milliseconds`
and `session_milliseconds` at `1333333` each ($0.08 / 60 000 ms, truncated —
$0.07999998/min), `requests` at `4000000000` ($0.004). A deployment's card must
carry all four: which of the audio pair or the clock a session reports depends
on the turn detection the customer chose, not on the deployment.

### Errors and credentials

Handshake refusals are classified by status as for OpenAI. xAI documents no
handshake status table; probed on 2026-09-30, no credential answered `401`
and an invalid one `400 {"code":"Client specified an invalid argument","error":"Incorrect API key provided..."}`.
That 400 labels itself an invalid argument and names no credential-specific
code, so it is `invalid_request` and the key is NOT retired on the strength of
its prose; a revoked key that xAI answers this way stays in its pool until a
`401`/`403` or an operator retires it (UNVERIFIED which status a revoked, as
opposed to malformed, key receives). xAI documents no insufficient-credit code
for the handshake; a `402` is read as the platform account refusing to be
billed, as everywhere.

### Sources

- https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech (guide, "OpenAI Realtime API Compatibility")
- https://docs.x.ai/developers/rest-api-reference/inference/voice (client and server events)
- https://docs.x.ai/voice-realtime.ws.json (machine-readable schema)
- https://docs.x.ai/developers/models/speech-to-speech (pricing, limits)
- https://docs.x.ai/developers/pricing , https://docs.x.ai/developers/rate-limits
- https://x.ai/news/grok-voice-think-fast-2 ($0.08/min for 2.0; the December 2025 launch post's $0.05/min is the retired 1.0 price)

No keyed xAI session has been opened from this repository. UNVERIFIED, for the
first signed canary: that `turn_detection: {"type": null}` (not `null`) is
accepted and yields push-to-talk billing; the function-tool shape xAI
enforces; whether `event_id` is echoed on `error.event_id` (the schema has the
field; client events document no `event_id`); whether `usage` is always on
`response.done`; the relative order of `session.created` and
`conversation.created` (the open waits for `session.created` either way).

Measured with the production key on 2026-09-30 by the publisher's discovery
probe, which opens and closes a session without writing to it: `session.created`
arrives first, unprompted, then `conversation.created`, then a JSON `ping`;
the opened session reports `turn_detection: {"type": null}` and
`voice: "xai_ara"`; `GET /v1/models` does NOT list
`grok-voice-think-fast-2.0`; and the handshake does not validate `?model=` —
a bogus id, and no id, are upgraded and answered with a `session.created`
naming `grok-voice-think-fast-2.0` (inventory.md, "xAI voice discovery").

Still unverified: under `server_vad`, whether xAI's self-created responses also
stream text (relayed if they do) and that a caller's speech interrupts them
without an `interrupt_response` field; and how closely `session_milliseconds`
(handshake to close, measured by Kaana) matches the duration xAI bills.

## Publishing

`configs/model-attribution.json` attributes `grok-voice-think-fast-2.0` to
`xai-realtime` only (`TestXAIVoiceIsAttributedOnlyToTheRealtimeAdapter`). xAI's
ids are classified by `providerconfig.ClassifyModel`: `grok-voice*` is a
conversation session, published only under a slug whose adapter opens one, so
it is dropped under `xai` and a text model is dropped under `xai-realtime`.

xAI's account list does not name its voice models, and its realtime catalogue
endpoints answer a team key 403, so `xai-realtime` is discovered with the
`xai_models_and_realtime_sessions` profile (`internal/publisher/xai_realtime.go`,
inventory.md, "xAI voice discovery"): for each voice id attributed under the
slug and absent from the list, the publisher opens
`wss://api.x.ai/v1/realtime?model=<id>` with the discovery key, sends nothing,
and discovers the id only when xAI's first `session.created` names exactly that
id. An `error` event or another model there leaves it absent; no answer fails
that cycle's `xai-realtime` discovery. The probe sends no event and no audio on
a session xAI opens as push-to-talk, which xAI's pricing bills only for audio
and `conversation.item.create` events.

`configs/model-attribution.json` attributes `gpt-realtime-2.1`,
`gpt-realtime-2.1-mini` and `gpt-realtime-2` to `openai-realtime` only. Each
model page lists the id as its own only snapshot, the rule `openai-audio`'s
`gpt-transcribe` already follows. The publisher classifies OpenAI's ids by what
they require (`internal/publisher/families.go`): Realtime ids are conversation,
transcription (`gpt-live-transcribe`, `gpt-realtime-whisper`) or translation
(`gpt-realtime-translate`) sessions; `gpt-live-1` is the separate GPT-Live
protocol and is inexpressible. A session model is attached only to a slug whose
adapter opens its kind, so today only Realtime conversation models, only under
`openai-realtime`. `TestOpenAIRealtimeIsAttributedOnlyToTheRealtimeAdapter`
pins the set and proves each row publishable there and nowhere else.

## Operating it

- Serve it: add `openai-realtime` to `KAANA_PROVIDERS`. Protocol and root are
  built in and locked; a `_BASE_URL` other than OpenAI's root, or the
  `openai_realtime` protocol under another slug, is refused at startup.
- Its key is an ordinary platform credential row under `openai-realtime`,
  imported like any other (`kaana-credentials put --provider openai-realtime …`
  or `kaana-platform-credential-import --provider openai-realtime …`) and bound
  per deployment with `kaana-credentials bind-deployment --provider
  openai-realtime`. It may be the same OpenAI account as `openai` and
  `openai-audio`; the pools still retire independently, each learning an
  account-wide `insufficient_quota` from its own refusal.
- Publish it: add it to `KAANA_DISCOVERY_PROVIDERS` with
  `KAANA_PROVIDER_OPENAI_REALTIME_DISCOVERY_KEY_ID`; it reads the same account
  list as the other OpenAI slugs.
- Health never opens a session (a probe that held one would be a charge) and
  stays `degraded` until a signed canary session with an explicitly funded key
  has been validated.
- Rate cards price the audio-token units per deployment
  (`audio_input_tokens`, `cached_audio_input_tokens`, `audio_output_tokens`
  beside the text units); without one, every attempt says its cost is unknown.
  OpenAI Realtime never reports `session_milliseconds`, so its card needs no
  rate for it (Oxy's price version still prices it, at zero).
- Sessions live in the memory of the task that opened them. A resume that the
  load balancer sends to another task is refused, by design (wire rule 7); the
  edge then recovers the report by `requestId` once the original task settles.
- The dedicated load balancer's idle timeout must exceed the 20 s ping
  interval (the ALB default of 60 s does). The Cloudflare request ceiling
  (`operating.md`) applies only to `/internal/v1/inference`.
- Logs carry ids, the route, the reason, units and cost — never a command, a
  transcript, an instruction or audio (`TestASessionOpensStreamsAndSettlesExactlyOnce`
  asserts it).

### Operating `xai-realtime`

- Serve it: add `xai-realtime` to the serving task's `KAANA_PROVIDERS` (with
  `xai`, which stays the request adapter). Protocol and root are locked; a
  `_BASE_URL` other than `https://api.x.ai/v1`, or `xai_realtime` under another
  slug, is refused at startup.
- Its key: a key row belongs to exactly one slug — a binding's foreign key is
  `(provider_slug, key_id)` (migration 0013) — so the `xai` row cannot be bound
  to an `xai-realtime` deployment, and no command copies a ciphertext row from
  one slug to another. Import an xAI API key under the new slug, from stdin,
  with the exact reviewed key id `05a6139b-5009-4d8f-b07c-4abccc79e349` (the
  id `.github/credential-admin-operations.json` names for discovery):
  `kaana-platform-credential-import --operation-id kpc_<32 hex> --provider
  xai-realtime --key-id 05a6139b-5009-4d8f-b07c-4abccc79e349 --class paid
  --position 1 …` (operating.md, "Container and deployment"), or
  `kaana-credentials put` with the same selectors from the admin task. It may
  be the same secret as the `xai` row (same team, same credit) or, preferably,
  a second key on the same xAI team so either can be revoked alone; the two
  pools retire independently either way. With exactly one key the provider
  default serves every `xai-realtime` deployment; bind explicitly with
  `kaana-credentials bind-deployment --provider xai-realtime` once there are two.
- Order matters: the serving process refuses to start on a `KAANA_PROVIDERS`
  slug with no enabled credential, and the publisher refuses to publish when a
  discovery key id names no enabled row — a publisher that stops writing lets
  every snapshot age past its one-hour horizon. The credential row therefore
  exists (and `kaana-credentials list` shows it) before the Terraform switch
  (`var.kaana_xai_realtime` in oxy-infra) adds `xai-realtime` to either set.
- Publish it: `xai-realtime` joins the publisher's `KAANA_DISCOVERY_PROVIDERS`
  through the same switch, with `KAANA_PROVIDER_XAI_REALTIME_DISCOVERY_KEY_ID`.
  The deploy workflow replaces the publisher's discovery key ids from the
  reviewed six-id map in `.github/credential-admin-operations.json` and reads
  them back; a key id for a slug the publisher does not discover is ignored,
  so the map may name `xai-realtime` before the switch is applied.
- Price it: a rate card for each published `xai-realtime` deployment with
  `audio_input_milliseconds`, `audio_output_milliseconds`,
  `session_milliseconds` and `requests` (above); without one every session's
  operator cost is unknown. A rate card is keyed by deployment id, and a
  deployment id carries the date the publisher first observed the line
  (`dep_xai_realtime_grok_voice_think_fast_2_0_observed_<date>`), so the card
  is written after the first snapshot names it, never guessed. It is
  `rc_xai_realtime_2026_09_30` in `configs/provider-rates.json`, for
  `dep_xai_realtime_grok_voice_think_fast_2_0_observed_2026_09_30`, read by the
  serving task through `KAANA_PROVIDER_RATES_PATH=/etc/kaana-rates/provider-rates.json`. Oxy's price
  version for the route must price `session_milliseconds` too (Oxy holds it for
  every realtime session and refuses a route that leaves it unpriced).
- A `server_vad` session needs `createResponse: true` and
  `interruptResponse: true`, audio among its output modalities, and xAI's
  bounds on the tunables; anything else is refused before anything is dialled.
  Health stays `degraded` until a signed canary session has been validated.

## Findings, refusals and what is not verified

1. **Transcription and translation sessions are refused** for `openai-realtime`
   (`unsupported_modality`, `kind`). OpenAI's translation is a separate endpoint
   (`/v1/realtime/translations`) and event protocol with no items, no
   responses and no usage event (billed per minute of audio); the contract's
   `output_audio.delta` needs a response and an item it would have to invent.
   OpenAI documents transcription sessions (`session.type: "transcription"`)
   but no WebSocket endpoint for them — only a beta-shaped REST route that
   mints a token. Both would be guesses from documentation.
2. **Input transcription in a conversation is refused.** It runs a second
   model (billed "according to the ASR model's pricing rather than the realtime
   model's"), and the contract gives a session one set of units. Reporting the
   ASR model's audio as `audio_input_milliseconds` beside the session model's
   `audio_input_tokens` is exactly the "same audio as tokens and as
   milliseconds" the contract's unit rule forbids; folding its tokens into the
   session's prices them at the wrong model's rate. The contract also says the
   transcription model is "the deployment's own, declared where the deployment
   is", and the inventory has no field for it. Both need a contract decision.
3. **`temperature` has no GA field** and is refused; the contract still carries
   it.
4. **Text and audio output together** is refused: GA takes exactly one output
   modality per response.
5. **Reasoning tokens** of the reasoning Realtime models are not split out of
   `output_tokens`: GA usage documents no reasoning breakdown. UNVERIFIED
   whether one exists on the wire.
6. **UNVERIFIED:** whether OpenAI refuses a bad key on the handshake (HTTP 401,
   as the adapter classifies) or in-band (an `error` event, also classified);
   the codes it uses in-band for quota and rate limits; what it sends when its
   own 60-minute session limit elapses (treated as whatever close follows).
7. **The models catalogue does not surface realtime capability.** The
   contract's `modelRealtimeCapabilities` is Oxy-owned catalogue metadata, and
   the inventory's `observed` block holds only what a provider's model list
   reported, which says nothing about sessions. Oxy can read the capability
   from the `openai-realtime` slug of a deployment; a first-class field needs an
   inventory decision.
8. **xAI substitutes an unknown model silently, and the serving open does not
   check.** Measured 2026-09-30: `?model=<bogus>` is answered with a
   `session.created` naming `grok-voice-think-fast-2.0`. The publisher
   discovers an id only when `session.created` names exactly it, but the
   adapter's `configure` accepts any `session.created` without comparing
   `session.model` to the signed upstream id. Today the only attributed id is
   xAI's default, so the two agree; the day they diverge (2.0 retired, the
   default moved) a session could run on other weights than its pinned
   reference names until the next publish drops the line. Refusing a
   `session.created` whose model differs is an adapter change not made here.
9. **A `requestId` is not remembered after its session settles.** Opening a
   second session under a settled request id is the edge's idempotency to
   refuse, exactly as a replayed one-shot envelope is (`architecture.md`,
   "Replay protection beyond the signature time window").

## Rules a reviewer applies

- **A realtime session is a `provider.RealtimeAdapter`, never a branch of a
  request adapter**, and a slug is exactly one kind. A request adapter is
  never handed a session and a session adapter never a request.
- **Routes are tried in signed order only until one OPENS**, through the same
  route resolution, breakers and exact key a request uses; after
  `session.created` nothing fails over and nothing is retried.
- **Credential verdicts come from the open, through `provider.WalkAttempts`**;
  nothing mid-session retires a key.
- **A command is acknowledged before it is applied, applied at most once, and
  never sent upstream twice** — not on a reconnect, not after a failed write.
- **A signed limit is checked before the command that would pass it is
  acknowledged**, and the event that would pass it is not delivered.
- **A session settles exactly once**: one `session.closed`, one usage report
  frame to the connection attached at that moment, one cost record per
  attempt. A reconnect settles nothing.
- **The wire in this document is fixed.** A change to it is a change the edge
  makes in the same rollout.
- **No command, instruction, transcript or audio enters a log line**, and the
  adapter's own key is redacted by exact match before any upstream text leaves
  it.
- **A session settles what its provider BILLS.** Units a provider reports but
  does not bill (xAI's voice tokens) are never settled; a provider billed by
  what Kaana can measure is metered through `provider.RealtimeMeter` and
  reported `oxy_measured`; a charge no contract unit expresses is refused,
  never approximated under another unit. A session billed by its wall clock
  (xAI `server_vad`) reports `session_milliseconds` and never an audio unit for
  the same span, and never changes billing mode once open.
- **A provider speaking the OpenAI Realtime shape is a dialect of
  `internal/provider/openairealtime`, not a copy**: its differences are
  `dialect` fields, each from the provider's documentation, and it gets its own
  slug, origin and fake (`openairealtimetest.NewXAI`).
