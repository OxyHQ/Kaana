# Realtime sessions

A realtime session is the one inference family that is not a request: a signed
session request opens a long-lived WebSocket conversation, the client keeps
sending commands, and Kaana keeps sending events, for up to an hour. It is one
request for attribution, metering and settlement, and it settles exactly once.
Contract: `@oxy.so/contracts` 4.4.0 (inference set 3.2.0), `inference/realtime.ts`;
Go: `internal/contract/realtime.go`, `realtime_wire.go`. Issue: OxyHQ/Kaana#90.

| Piece | Where |
|---|---|
| The edge↔Kaana wire, below | `internal/realtime` (`GET /internal/v1/realtime`) |
| Routing a session open, settling it | `kaana.Executor.OpenSession`, `SettleSession` |
| The provider-side session interface | `provider.RealtimeAdapter`, `provider.RealtimeUpstream` |
| OpenAI Realtime (GA) | `internal/provider/openairealtime`, slug `openai-realtime` |
| The real-wire fake OpenAI upstream | `internal/provider/openairealtime/openairealtimetest` |

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
   reported, each unit once;
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

## Publishing

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
- Sessions live in the memory of the task that opened them. A resume that the
  load balancer sends to another task is refused, by design (wire rule 7); the
  edge then recovers the report by `requestId` once the original task settles.
- The dedicated load balancer's idle timeout must exceed the 20 s ping
  interval (the ALB default of 60 s does). The Cloudflare request ceiling
  (`operating.md`) applies only to `/internal/v1/inference`.
- Logs carry ids, the route, the reason, units and cost — never a command, a
  transcript, an instruction or audio (`TestASessionOpensStreamsAndSettlesExactlyOnce`
  asserts it).

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
8. **A `requestId` is not remembered after its session settles.** Opening a
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
