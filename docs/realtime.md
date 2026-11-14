# Realtime sessions

A realtime session is the one inference family that is not a request: a signed
session request opens a long-lived WebSocket conversation, the client keeps
sending commands, and Kaana keeps sending events, for up to an hour. It is one
request for attribution, metering and settlement, and it settles exactly once.
Contract: `@kaana.ai/contracts`, `src/{public,internal}/realtime.ts` (the session
family arrived in inference set 3.2.0, `session_milliseconds` in 3.3.0); input
transcription and the realtime catalogue fields are `@kaana.ai/contracts`
4.1.0 ("Input transcription"); since contract set 6.0.0 one canonical API
whose every value Kaana translates or emulates per provider (Kaana ADR 0003,
"How Kaana serves every contract value");
Go: `internal/contract/realtime.go`, `realtime_wire.go`. Issue: OxyHQ/Kaana#90.

Voice mode moves to the `live` session kind, whose turns the caller's backend answers
(native delegation on OpenAI GPT-Live, emulated on the speech-to-speech providers below):
[ADR 0012](adr/0012-live-voice-sessions-with-delegation.md). Its contract and the session's half are
built (contract set 8.16.0, "Live sessions" below). Its native adapter is OpenAI GPT-Live
("OpenAI GPT-Live: `openai-live`"), and `openai-realtime` and `xai-realtime` serve it by emulation
("Emulated delegation").

| Piece | Where |
|---|---|
| The edge↔Kaana wire, below | `internal/realtime` (`GET /internal/v1/realtime`) |
| Routing a session open, settling it | `kaana.Executor.OpenSession`, `SettleSession` |
| The provider-side session interface | `provider.RealtimeAdapter`, `provider.RealtimeUpstream` |
| OpenAI Realtime (GA) | `internal/provider/openairealtime`, slug `openai-realtime` |
| xAI Voice Agent (the same adapter, xAI's dialect) | `internal/provider/openairealtime` (`dialect.go`, `meter.go`), slug `xai-realtime` |
| `live` by emulated delegation (both dialects) | `internal/provider/openairealtime/live.go`; the shared preamble and verbatim marks in `internal/provider/live.go` |
| The real-wire fake upstreams | `internal/provider/openairealtime/openairealtimetest` (`New`, `NewXAI`) |
| OpenAI GPT-Live, the native `live` voice | `internal/provider/openailive`, slug `openai-live`; fake `openailivetest` |
| OpenAI's error vocabulary, shared by both OpenAI wires | `internal/provider/openaierror` |
| The `live` contract every live adapter passes | `TestLiveContract` (`internal/realtime`) |

## The wire between the edge and Kaana

This is fixed: the Oxy edge is built against it. It is reproduced exactly.

> **Kaana realtime session wire (edge <-> Kaana), binding for both sides**
>
> Contract: `@kaana.ai/contracts`, module `src/internal/realtime.ts`
> (realtimeSessionRequestSchema, realtimeClientCommandSchema, realtimeServerEventSchema).
> Go types: Kaana `internal/contract/realtime.go`, `realtime_wire.go`.
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
>    In a configured ECS deployment, another task relays `session.resume` once to that
>    owner through the private, generation-verified path below. An unavailable owner,
>    a resume after `resumeWindowMs`, or an `afterSequence` no longer buffered is answered with
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
- **A command the session's kind does not take** (`session.update`,
  `input_audio.commit` and `input_audio.clear` on a `live` session; a
  `delegation.*` command on any other kind) is refused with a non-fatal
  `invalid_request`, param `type`, before it is acknowledged: nothing is
  applied and nothing reaches the provider (`contract.RealtimeSessionKind.Takes`).
- **A `delegation.*` command naming a delegation that is not the open one** is
  refused the same way, param `delegationId`, after the duplicate check ("Live
  sessions").

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
   and labelled `oxy_measured` on `session.closed` and the report; for a
   provider that reports the whole session's usage once, at its end
   (`provider.RealtimeFinalUsage`, GPT-Live), that report, read once here
   with the source the adapter could confirm, after the trailing events only
   that end produces (the `transcript.done` of an item still open);
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

A `live` session is served on the same two interfaces ("Live sessions"): `Next`
returns `delegation.requested`, `Send` receives the `delegation.*` commands.

It is not `provider.Adapter`, and a slug is exactly one of the two: the registry
refuses a type that implements both, one that implements neither, and a session
adapter that declares no kind (`TestASlugIsExactlyOneKindOfAdapter`, with its
positive controls). An adapter declares its kinds from
`providerconfig.RealtimeSessionKinds`, the session counterpart of
`ExecutableAPIFormats`, which the publisher reads too.

What a session adapter does not do is Adapter's list: it allocates no id,
numbers nothing, decides no terminality beyond reporting that its upstream
ended, and never decides what a failure means for the key.

## Live sessions

Contract set 8.16.0 ([ADR 0012](adr/0012-live-voice-sessions-with-delegation.md) §1, phase K1)
adds the `live` kind: the voice speaks, detects turns and hands each turn to the caller's backend,
which answers it with its own model and tools. The contract is in `src/public/realtime.ts`
(`realtimeConfigIssues`, `realtimeDelegation*`, `realtimeSessionTakes`); Go mirrors it in
`internal/contract` (`RealtimeLive`, `RealtimeDelegationConfig`, `RealtimeDelegation*Command`,
`RealtimeDelegation*Event`, `RealtimeSessionKind.Takes`, `DelegationOf`).

| | |
|---|---|
| Configuration | `voice`, `outputAudioFormat` equal to `inputAudioFormat`, `turnDetection` other than `none`, and `delegation {acknowledgement: auto \| none}` are required; `outputModalities`, `tools`, `toolChoice`, `maxOutputTokens`, `translation` and an input transcription `prompt` or `interimResults` are refused. `delegation` is refused on every other kind |
| Refused commands | `session.update`, `input_audio.commit`, `input_audio.clear` (param `type`, unacknowledged) |
| Commands | `delegation.reply {delegationId, text, verbatim?, final}`, `delegation.context {delegationId, text}`, `delegation.cancel {delegationId}` |
| Events | `delegation.requested {delegationId, text}`, `delegation.superseded {delegationId}` |
| Catalogue | a line lists `live` in `capabilities.realtime.sessionKinds`; `defaultKinds` names the kinds it is Kaana's default for ("What the catalogue says about a realtime model") |

**The session owns the delegation** (`internal/realtime`), so every adapter shares one rule. The
open delegation is the newest `delegation.requested` an adapter returned, until its `final` reply or
its `cancel` is applied. A `delegation.requested` while one is open is preceded by
`delegation.superseded` for it, numbered by the session's one goroutine; an adapter never returns
`delegation.superseded` (one that did is logged and not relayed). A `delegation.*` command naming any
other delegation is refused, unacknowledged, param `delegationId`; one for the open delegation is
acknowledged and sent upstream like any command, and an upstream that cannot express it answers
`provider.ErrUnsupported`, a non-fatal refusal that leaves the delegation open.

**Who serves it.** Phase K2 adds the native GPT-Live adapter (`openai-live`, below), phase K3 the
emulation on `openai-realtime` and `xai-realtime`. Each implements the provider side documented on
`provider.RealtimeUpstream` and passes `TestLiveContract` (`internal/realtime`), which runs the whole
canonical contract against every live adapter on its own real-wire fake, through the data plane on
kaana.ai/v1: open, audio both ways, transcripts both ways, a delegation answered in two replies with a
verbatim span, supersession, cancel, the refusals above, close and exactly-once settlement in the
adapter's own units. An adapter joins it by one entry in `liveFixtures` and its fixture beside it.
`TestALiveSessionHandsItsTurnsOverAndRefusesWhatItsVoiceOwns` still runs the session's half against a
scripted upstream. `configs/public-catalogue.json` declares `openai/gpt-live-1` as the default
`live` line ("Operating `openai-live`").

K3 lists `live` in the statements of `openai-realtime` and `xai-realtime` ("Emulated delegation"), so
the inventory publishes it in the `sessionKinds` of their lines (`x-ai/grok-voice-3.0`, the
`gpt-realtime` lines), none of them a default. `configs/public-catalogue.json` still declares
`sessionKinds: ["conversation"]` for those lines: the
declared catalogue is what Oxy parses (`declaredCatalogueSchema`), and Oxy on a contracts package older
than 8.16.0 refuses the whole document when a line names `live`. The served kinds come from the
inventory either way. `GET /v1/models` lists `live` only to a reader whose
`Kaana-Contract-Version` is 8.16.0 or later (`CATALOGUE_ADDITIONS`, contract set 8.17.0,
`contract.md`): an older reader's closed `realtimeSessionKindSchema` would refuse the whole list, so
it is served those lines with `conversation` alone.

The signed-envelope lane refuses a `live` session at open (`kaana.EnvelopeLiveRefused`, param
`kind`): its voice transcribes the caller whatever the configuration says, and on OpenAI that is
billed as `input_transcription_milliseconds`, a unit the lane's consumer cannot settle.

## Emulated delegation (`openai-realtime`, `xai-realtime`)

Neither OpenAI-Realtime-shaped wire hands a turn to its caller, so a `live` session is served as a
conversation whose one tool hands it over (ADR 0012 §2, `internal/provider/openairealtime/live.go`).
Both dialects run the same code; what their wires spell differently is the dialect's, exactly as for
a conversation. `live` is an emulated kind there (`providerconfig.RealtimeSessionKinds` lists it, after the
kinds the protocol's models are): the publisher matches a session model's own kind against
`RealtimeModelKinds` only, so the conversation lines (`gpt-realtime-*`, `grok-voice-*`) serve `live`
and the native `gpt-live-1` is never attached to `openai-realtime`
(`TestGPTLiveIsAttributedOnlyToTheLiveAdapter`).

**Configuration** (`dialect.liveConversation`, then the conversation's own `wireSession`):

| Wire field | Value |
|---|---|
| `instructions` | `provider.LivePreamble`, a blank line, the caller's persona (`provider.LiveInstructions`). The preamble is shared with the native adapter, word for word (`internal/provider/live.go`) |
| `tools` | exactly one: `{type: "function", name: "delegate", description, parameters: {type: object, properties: {request: {type: string}}, required: [request]}}`. The description says to call it for every request, what its result holds and, by `delegation.acknowledgement`, whether one very short acknowledgement ("one moment") may precede the call (`auto`) or nothing (`none`) |
| `tool_choice` | `auto`; nothing on xAI, whose only choice it is |
| output | audio only (OpenAI `output_modalities: ["audio"]`; on xAI each `response.create` asks `modalities: ["audio"]`), the session's voice and format |
| turn detection | the caller's `server_vad` / `semantic_vad`, translated as for a conversation, with `create_response` and `interrupt_response` sent `true` whatever the caller asked: the voice answers every turn by delegating and is interruptible. On xAI they are fixed, so `false` is refused as Unservable says, before a grant |
| input transcription | always on, with the caller's language when it named one; the deployment's transcription model as for a conversation |

**Events.**

| Provider | Contract |
|---|---|
| The completed call to `delegate` (`response.function_call_arguments.done`; the name from the call's output item, or on the event itself on xAI) | `delegation.requested {delegationId: call_id, text}`. `text` is the caller's final input transcripts since the previous delegation, joined by a space; when none arrived, the voice's `request` argument |
| The call while a committed turn of the caller's has no final transcript yet | held, and handed over once every committed turn has its final transcript, its transcription failed, or the caller starts talking again (`input_audio_buffer.speech_started`). OpenAI transcribes asynchronously, so its call can come first; xAI sends the final transcript right after the commit, before the response's output (testdata) |
| A second call while one is held | the held one is handed over first, with what was heard; the session supersedes it |
| Input and output transcripts, output audio deltas, `error` | relayed as for a conversation; `⟦` and `⟧` are removed from the voice's transcripts (`provider.StripVerbatimMarks`), and a delta left empty is not sent |
| Everything else: `response.*`, items, `tool_call`, speech and commit markers, `output_audio.done` | not relayed (GPT-Live has no counterpart); the units an event carried are still reported, as a units-only event |
| A call to any other function name | dropped |

**Commands.**

| Contract | Provider |
|---|---|
| `input_audio.append` | `input_audio_buffer.append`, as for a conversation |
| `delegation.reply` (not final) | buffered: the parts are joined with a space where neither side has one, each `verbatim` span wrapped in `⟦` `⟧` (`provider.WrapVerbatim`); nothing is sent |
| `delegation.context` | buffered with the delegation's answer |
| `delegation.reply` (final) | `conversation.item.create {type: function_call_output, call_id, output}` then `response.create`, both with `event_id` = the final reply's `commandId`. `output` is the reply, or with context `Context, not to be said: <context>`, a blank line, then the reply. A reply longer than a function output carries (1 048 576 UTF-16 units) is refused, `text`, and the earlier parts stay |
| `delegation.cancel` | `conversation.item.create` of the call's output `This request was dropped. Say nothing about it.`, with no `response.create`, so nothing is spoken for it. Closing the call with an unspoken output is the closest the wire allows to dropping it |
| any other command (`conversation.item.*`, `response.*`) | refused, param `type`, unsent: a `live` session exposes no conversation |

**Billing** is the dialect's, unchanged (ADR 0012 §5): OpenAI settles the tokens each `response.done`
reports (the call's response and the spoken answer's) and the transcription model's
`input_transcription_milliseconds`, both relayed as units even though the events are not. xAI's
`server_vad` session is metered by its clock, `session_milliseconds`; a function output and
`response.create` are not billed by xAI and add no `requests` (`meter.measure` names neither).
The emulation adds no unit.

**Tests** (`live_test.go`, both dialects): the configuring `session.update`, one whole turn with a
two-part reply, context and a verbatim span (output, relayed events and units exact), the wait for a
turn's transcript, an untranscribed turn, a call with no turn, the release when the caller talks on,
cancel without `response.create`, a newer call's own answer, the refused conversation commands, and a
replay of the measured xAI captures (`xai-ended-turn.jsonl`, `xai-interrupted-turn.jsonl`) with the
call spliced into xAI's response. `TestTheDialectTranslatesEveryContractValue` covers every live
value on both dialects. The fakes script the call and read the answer back
(`openairealtimetest.Upstream.Delegate`, `DelegateCall`, `Conn.ExpectOutput`). What no fake can prove
(how faithfully each voice speaks a function output, and verbatim spans) is in
[realtime-findings](realtime-findings.md).

## OpenAI GPT-Live: `openai-live`

GPT-Live is a full-duplex voice that listens, speaks, decides when a turn ends and hands every turn to
its client ("client delegation"). It is the native adapter of the `live` kind ([ADR 0012](adr/0012-live-voice-sessions-with-delegation.md)
§2), under its own slug for the reason `openai-realtime` is one: its protocol is neither a request nor
the Realtime API, and a slug resolves to exactly one adapter.

| | |
|---|---|
| Protocol | `openai_live` (only under the `openai-live` slug; refused under any other at startup) |
| Configured root | `https://api.openai.com/v1`, locked; the publisher reads the account's `GET /v1/models` there, which lists `gpt-live-1` for the production key (2026-11-14) |
| Session endpoint | `wss://api.openai.com/v1/live/sessions`, no query parameters, fixed in `providerconfig` |
| Authentication | `Authorization: Bearer <key>` on the handshake |
| Session kinds | `live` only |
| Model | `gpt-live-1`, its only snapshot |

**Opening.** Dial (a non-101 handshake is classified from its status and OpenAI's error body), send
`session.start` as the first frame (`event_id: kaana-session-start`), and wait for `session.started`;
only then is audio sent. An `error` before it is OpenAI's answer about this session on this key and is
classified like a refused request. Each stage is bounded at 20 s. Everything is fixed at start:

| Contract | GPT-Live `session.start.session` |
|---|---|
| (the route) | `model`: the signed upstream id |
| `instructions` | `instructions`: `provider.LivePreamble`, then the persona (`provider.LiveInstructions`) |
| `inputAudioFormat` = `outputAudioFormat` | `audio.format`: `pcm16_24khz` → `{type: "audio/pcm", rate: 24000}`; `g711_ulaw` → `{type: "audio/pcmu", rate: 8000}`; `g711_alaw` → `{type: "audio/pcma", rate: 8000}` (GPT-Live's 16 kHz PCM has no contract format) |
| `voice` | `audio.output.voice`: one of the 38 built-in ids; any other is refused (`config.voice`) before dialling |
| `delegation.acknowledgement` | `delegation: {type: "client", acknowledgement_delay_ms}`: `auto` 400, `none` 0 |
| (always) | `store: false`: no recording is kept for forking |
| `turnDetection`, its thresholds, `inputAudioTranscription.language` | not sent: the voice detects the turns, and GPT-Live documents no setting for either |

**Commands.**

| Contract command | GPT-Live client event (`event_id` minted by Kaana, `kaana-<n>`, and mapped back to its command for an `error`) |
|---|---|
| `input_audio.append` | `session.input_audio.append {audio}` (not acknowledged by OpenAI); PCM16 that is not whole 16-bit samples is refused (`data`) |
| `delegation.reply` | `session.commentary.append {delegation_id, content}`, spoken (paraphrased): the text with each `verbatim` span between `⟦ ⟧` (`provider.WrapVerbatim`), cut into appends of at most 500 bytes at the last sentence end that fits, else the last space, never inside a span |
| `delegation.context` | `session.thinking.append {delegation_id, content}`, silent, cut the same way |
| `delegation.cancel` | `session.instructions.append {delegation_id, content}`: the turn is dropped and nothing more is said about it (an instructions append "can interrupt current speech") |
| (each delegation of an `acknowledgement: none` session) | an empty `session.thinking.append` naming it, which cancels the pending acknowledgement |
| `session.close` | answered by the session; at settlement Kaana sends `session.close` and waits for `session.closed` |
| anything else | refused (`type`), nothing written |

GPT-Live bounds each append at 500 tokens and documents no tokenizer. A token is never shorter than one
byte of UTF-8, so 500 bytes is at most 500 tokens in any script: a bound that is certain where a
characters-per-token estimate is not. A span (at most 256 UTF-16 units) is never cut; one longer than
the bound is an append of its own.

**Events.**

| GPT-Live server event | Contract event |
|---|---|
| `session.input_transcript.delta` | `transcript.delta`, `source: input_audio`, item `input_<n>`: relayed whether or not transcription was asked for, as the `live` kind requires |
| `session.delegation.created` | the input item's `transcript.done` (its whole text), then `delegation.requested` with that text and OpenAI's delegation id (1–128 units, unique; target `client`) |
| `session.output_transcript.delta` | `transcript.delta`, `source: output_audio`, item `output_<n>`, response `response_<n>` |
| `session.output_audio.delta` | `output_audio.delta`, split into frames of at most `provider.MaxAudioChunkBytes`, in the open utterance |
| (an utterance ends) | its `transcript.done`: a fragment starting 1.5 s or more after the last one ended on OpenAI's session clock, 1.5 s with no output on Kaana's, or the session's end, where it and the caller's open item are trailing events emitted before `session.closed`. No `output_audio.done`: GPT-Live has none |
| `error` | `error{fatal:false}`, `commandId` from `client_event_id`; a failure other than `invalid_request_error` is the session's own and is how it ends if the connection then closes |
| `session.closed` | the end: `close_requested`, `expired`, `remote_hangup` → `upstream_closed`; `content` → `upstream_error` (`upstream_content_filtered`); `connection_lost` → `upstream_error` (`provider_error`) |
| `session.started` (after open), `session.updated`, `*.appended`, `session.input_audio.muted`/`unmuted`, `session.usage.updated`, `info`, Responses and Decisions events | dropped: the contract has no event for them, and Kaana configures neither delegation type |

`⟦` and `⟧` are never relayed: they are stripped from every transcript, the caller's and the voice's.
GPT-Live has no items, responses or turn ends, so Kaana numbers them: the caller's words since the
previous delegation are one item, and each utterance of the voice is one item and one response.

**Usage.** GPT-Live bills the session's seconds, $0.05 a minute, per second, never rounded up to the
minute. `session.usage.updated` is a cumulative snapshot ("do not sum across events") and is not
settled; `session.closed.usage.seconds` is the final figure, read as an exact decimal and rounded up
once to whole `session_milliseconds`, `provider_reported`. A session Kaana ends asks for it
(`session.close`, then up to 10 s for `session.closed`); a session OpenAI ends sends it. Without
`session.closed` the usage is unconfirmed (OpenAI: "If the transport fails before session.closed, final
usage is unconfirmed"), so the session settles Kaana's own wall clock, from the accepted handshake to
the end of the connection, rounded up once, `oxy_measured`: the clock bounds what OpenAI can bill. The
adapter is a `provider.RealtimeFinalUsage`: its events carry no units, and the session reads the final
usage once, at settlement.

**Errors.** By OpenAI's own type and code through `internal/provider/openaierror`, the one table both
OpenAI wires read: handshake 401/403 → `provider_credential_invalid`; 402, or 429 with the quota
vocabulary → `provider_billing_refused`; 429 → `rate_limited`; 404 → `model_not_found`; 408/504 →
timeout; 503 → overloaded; 5xx → `provider_error`. Every message is stripped of the key by exact match
before `contract.SafeErrorText`. No prompt, persona, transcript, reply or audio enters a log line.

**Not verified** (the documentation does not say, so the fake cannot know; marked UNVERIFIED in the
code): the handshake's refusal statuses and bodies; GPT-Live's in-band error vocabulary beyond
`invalid_request_error` / `unknown_parameter`; whether an acknowledgement delay of 0 requests one at once
or none (the empty thinking append covers either); and whether a turn's input transcript always
arrives before its `session.delegation.created` (a fragment after it is the next turn's).

Sources, read 2026-11-14:
[live conversations](https://developers.openai.com/api/docs/guides/live-conversations),
[live delegation](https://developers.openai.com/api/docs/guides/live-delegation),
[WebSocket](https://developers.openai.com/api/docs/guides/voice-websockets?api=live),
[primary WebSocket reference](https://developers.openai.com/api/reference/resources/live/primary-websocket),
[gpt-live-1](https://developers.openai.com/api/docs/models/gpt-live-1).

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
| Session kinds | `conversation`, and `live` by emulated delegation ("Emulated delegation") |

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
| `outputModalities` `["audio"]` / `["text"]` | `output_modalities` `["audio"]` / `["text"]`; `["text","audio"]` is sent as `["audio"]` and the audio's transcript is relayed as the text too (OpenAI produces one or the other) |
| `inputAudioFormat` `pcm16_24khz` / `g711_ulaw` / `g711_alaw` | `audio.input.format` `{type:"audio/pcm",rate:24000}` / `{type:"audio/pcmu"}` / `{type:"audio/pcma"}` |
| `outputAudioFormat`, `voice` | `audio.output.format`, `audio.output.voice` |
| `turnDetection` `none` | `audio.input.turn_detection: null` |
| `turnDetection` `server_vad` | `{type:"server_vad", threshold?, prefix_padding_ms?, silence_duration_ms, create_response, interrupt_response}`; a session that names no silence is sent Kaana's `REALTIME_DEFAULT_SILENCE_DURATION_MS` (500), never the provider's default |
| `turnDetection` `semantic_vad` | `{type:"semantic_vad", eagerness, create_response, interrupt_response}` |
| `tools` (function) | `tools: [{type:"function", name, description?, parameters}]`; a realtime tool has no `strict` (contract set 5.0.0: `realtimeToolDefinitionSchema`) |
| `toolChoice` | `tool_choice`: the mode string, or `{type:"function", name}` |
| `maxOutputTokens` | `max_output_tokens`, at most 4096 (GA schema: 1..4096 or `"inf"`): a larger cap is sent as 4096 |
| `inputAudioTranscription` | `audio.input.transcription {model: "gpt-transcribe", language?, prompt?}`; the language as its ISO 639-1 code (see "Input transcription") |

There is no `temperature`: OpenAI removed it from the GA session and response
schemas and xAI has none, so contract set 5.0.0 removed it from the contract.

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
| `conversation.item.input_audio_transcription.delta` / `.completed` | `transcript.delta` / `.done`, `source: input_audio`; `.completed` carries the ASR model's units (see "Input transcription") |
| `conversation.item.input_audio_transcription.failed` | `error{fatal:false}`, naming no command: one turn was not transcribed, the session continues |
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
| Session kinds | `conversation` (xAI documents no other), and `live` emulated on it ("Emulated delegation") |
| Model | `grok-voice-3.0` (attributed 2026-10-16); `grok-voice-latest` is xAI's moving alias and `grok-voice-2.0` is deprecated, so neither is attributed. `grok-voice-think-fast-2.0`, attributed until then, stopped being served on 2026-10-14 (item 10 below) |
| Turn detection | `none` (push-to-talk, billed by audio) or `server_vad` (billed by session wall clock) — see "Metering" |
| Session limit | xAI's own: 120 minutes, 10 concurrent sessions per team at tier 0 |

### What differs from OpenAI's dialect

| | OpenAI GA | xAI |
|---|---|---|
| `session.type` | `"realtime"` | absent from xAI's schema; not sent |
| Voice | `audio.output.voice` | `session.voice` |
| Turn detection | `audio.input.turn_detection`; none is `null` | `session.turn_detection`; none is `{"type": null}` |
| `server_vad` | `create_response`, `interrupt_response` sent | only `threshold` (0.1–0.9), `prefix_padding_ms` and `silence_duration_ms` (0–10000); `createResponse` and `interruptResponse` must be `true` (xAI's VAD always answers and is interruptible; it has neither field) |
| `server_vad` threshold | 0–1 | 0.1–0.9: the contract's 0–1 is mapped linearly onto it (0.5 stays 0.5, 0.7 is 0.66) |
| `server_vad` silence | the session's, or Kaana's 500 ms | the same, as `silence_duration_ms`, which xAI honours though its schema does not name it (measured 2026-10-16: with xAI's own default a phrase with 0.56 s and 0.79 s pauses became three responses, two cancelled; at 500 ms, one, 490–576 ms after it ended) |
| `semantic_vad` | served | run as xAI's `server_vad` (its schema names only `server_vad` or null); the eagerness is not sent |
| Output modalities | `session.output_modalities`, one per response | no documented session field: the session's modalities are sent as `response.modalities` on every `response.create` the client does not override (every response of a push-to-talk session is created by one); a `server_vad` response xAI creates itself carries xAI's own modalities. xAI answers aloud whatever it is asked (measured, below), so a set without `audio` is served by asking for `["audio"]`: the audio is measured (billed) and not relayed, and its transcript is the response's `text.delta` |
| `response.status_details` | an object (`{type, reason}`) or `null` | the string `"unimplemented"` on `response.created` and `response.done` (measured): read as no details, so an incomplete xAI response has no finish reason. Any other non-object is unreadable, and OpenAI's dialect still refuses a string |
| `tool_choice`, `max_output_tokens` | served | none documented. `toolChoice: "auto"` is xAI's own behaviour with the tools it was given, sent as nothing; `none` for the session is served by sending `tools: []`; `required`, a named function, or `none` on one `response.create` cannot be served and are refused naming the field. `maxOutputTokens` is not sent |
| Function tools | flat `{type, name, description, parameters}` | the guide's examples are flat; the machine-readable schema nests under `function` (UNVERIFIED which xAI enforces; flat is sent) |
| Assistant content parts | `output_text` / `output_audio` | `text` / `audio` (read back as the contract's output parts for the assistant, input parts otherwise) |
| Event aliases | — | `response.text.delta` = `response.output_text.delta` ("Functionally identical... handle both"); `response.audio.delta` = `response.output_audio.delta` |
| `conversation.item.done` | sent | not emitted by xAI; the contract event simply does not occur |
| `response.done` of a talked-over answer | sent, `cancelled` | not sent: xAI opens the next response with none for the first (measured 2026-10-16). Both run one in-conversation response at a time, so a `response.created` while one is open ends it: Kaana reports that one `response.done` `cancelled`, with no usage (xAI's session is metered), before the new `response.created` |
| In-band errors | OpenAI's type/code vocabulary | `invalid_request_error` / `invalid_event` refuse one event (session continues); `internal_error` is xAI's failure; `timeout` / `max_duration` end the session (`provider_timeout`) |
| Handshake refusal body | `{"error": {type, code, message}}` | `{"code": "<status text>", "error": "<message>"}` (probed, see below) |
| Usage | `response.done.usage` tokens, billed | tokens reported but NOT billed: never settled; the session is metered instead |

Input transcription is `audio.input.transcription {model: "grok-transcribe",
language_hint?}` (see "Input transcription"). xAI's extensions
(server-side `web_search`/`x_search`/`file_search`/`mcp` tools, `force_message`,
resumption, `replace`, binary transport, `idle_timeout_ms`,
`reasoning.effort`) have no contract field and are never sent; `reasoning.effort`
therefore stays at xAI's default (`high`).

### The wire xAI actually sends (measured 2026-09-30)

The first production session through Oxy → Kaana → xAI (requestId
`253dacf6-2ea5-4c46-9afd-ca87a9a9ec1a`, `grok-voice-think-fast-2.0`,
`outputModalities: ["text"]`, push-to-talk; and `e5c429ad-…`, the same flow
spoken) opened, relayed the user's `conversation.item.added`, and then ended
`upstream_error` with "xAI sent a Realtime event this adapter cannot read" —
after xAI had billed the $0.004 text item. The cause was the event every xAI
response starts with: `response.created` carries `"status_details":
"unimplemented"`, a string where OpenAI's schema (and the adapter's struct) has
an object or null, so the whole event failed to decode. A raw capture of the
same flow, replayed verbatim through the adapter by
`TestXAICapturedResponseIsReadAndMetered`, is
`internal/provider/openairealtime/testdata/xai-text-only-capture.jsonl`. What it
shows, beside what xAI documents:

| Event | Measured shape | How it is read |
|---|---|---|
| `ping` | `{"type":"ping","timestamp":<ms>,"previous_item_id":null}`, before and between turns | dropped (the contract has no such event) |
| `session.updated` | `turn_detection: {}` (sent `{"type":null}`), `input_audio_format` / `output_audio_format: "not specified"`, `temperature: -1`, `max_response_output_tokens: "inf"`, and the `modalities: ["text"]` the probe set echoed back | only its arrival is read: it confirms the pending update |
| `response.created` | `status_details: "unimplemented"`, `usage: {}`, `output: []` | `status_details` per the dialect (above); `usage` is never read for xAI |
| `conversation.item.added` (assistant) | `content: []`, `status: "in_progress"`, `replayed: false` | a message with no content has no contract shape and is not forwarded |
| `response.content_part.added` | `part: {"type":"audio","transcript":""}`, `previous_item_id: "0"` | dropped, as every content part event is |
| `response.output_audio.delta` | extra `rid`, `latency` (a string), `audio_duration_ms`, `ts` | relayed and metered by its decoded bytes; the extras are ignored |
| `response.output_audio_transcript.delta` / `.done` | the spoken answer's transcript | relayed as transcript events (`source: output`) |
| `response.done` | `status_details: "unimplemented"`, `response.usage: {}`; a top-level `usage` with `input_token_details.grok_tokens`, `output_token_details.audio_tokens`, `output_audio_seconds: 0.71`, `billable_audio_seconds: 1` | status read; the tokens are never settled (the session is metered) |

**xAI answers aloud whatever it is asked.** The capture set `modalities:
["text"]` on the session (xAI echoed it on `session.updated`) and still got an
`audio` part, three output audio deltas (710 ms, exactly xAI's
`output_audio_seconds: 0.71`), their transcript and `billable_audio_seconds: 1`
— no `response.output_text.delta` at all. Since contract set 6.0.0 (Kaana ADR
0003) a text-only session on xAI is served from that audio: Kaana asks xAI for
`["audio"]` in PCM16 (`dialect.wireOutputFormat`), measures the audio xAI sends
(it is billed, so the meter has an output rate), relays none of it, and relays
its transcript as the response's `text.delta`. The session's price is the
spoken one, which `pricePerMinute` already states. The capture put
`modalities` on the session, not on `response.create` (where Kaana sends it);
UNVERIFIED whether xAI's documented per-response field produces text alone —
were it to, the emulation would be replaced by the wire's own text.

An event the adapter cannot read is now named in the failure's own detail —
`xAI sent a Realtime event this adapter cannot read (response.created:
response.status_details is a JSON string)` — the event type (kept only if it
looks like one) and the field path in the adapter's own vocabulary with the
JSON kind found there, never a value the provider sent. That detail is the
fatal error's message and, with `errorCode`, on Kaana's `realtime session
closed` log line for a session that ended `upstream_error`.

UNVERIFIED: `billable_audio_seconds: 1` beside `output_audio_seconds: 0.71`
suggests xAI bills audio in whole seconds (per response, per session or per
invoice line is not known). Kaana meters milliseconds, rounded up once over
the session, so a session of many short answers may measure less than xAI
bills; the first signed canary should compare the two.

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

The capture above is the one keyed xAI conversation measured so far: xAI
accepted `turn_detection: {"type": null}` (and reported it back as `{}`), and
`response.done` carried usage at the event's top level with `response.usage`
empty. UNVERIFIED, for the first signed canary: that push-to-talk is billed by
audio as documented; the function-tool shape xAI enforces; whether `event_id`
is echoed on `error.event_id` (the schema has the field; client events document
no `event_id`).

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

## Input transcription

Contract set 4.1.0 (Alia ADR 0017, Kaana phase 1). A conversation that sets
`config.inputAudioTranscription` (at open, or by a `session.update` the
provider confirms) receives the caller's speech, turn by turn, as
`transcript.delta` / `transcript.done` with `source: input_audio`, on the
caller's own item. The contract says the transcription model is "the
deployment's own, declared where the deployment is"; every conversation model
of a slug shares it, so the dialect declares it (`openairealtime/dialect.go`,
`transcriptionDialect`), with its evidence, and changing it is a reviewed
change to what the slug's sessions are billed for.

| | OpenAI (`openai-realtime`) | xAI (`xai-realtime`) |
|---|---|---|
| Wire | `session.audio.input.transcription {model, language, prompt}` | `session.audio.input.transcription {model, language_hint}` |
| Model | `gpt-transcribe` (per-minute priced, $0.0045/min, read 2026-10-15) | `grok-transcribe` |
| `language` | the client's BCP 47 tag as its ISO 639-1 code (`es-AR` → `es`) | xAI's documented spelling (`providerconfig.XAIRealtimeLanguages`): the tag itself, the language alone, or xAI's default region (`es` → `es-MX`, `pt` → `pt-BR`, `ar` → `ar-SA`) |
| A language it has no code for | not sent: the model detects it | the same (xAI silently ignores an unknown hint, so none is sent) |
| `prompt` | sent | not sent: a hint, and xAI has key terms, not a prompt |
| Events | `.delta` relayed; `.completed` → `transcript.done`; `.failed` → non-fatal error; `.segment` (diarize) dropped | cumulative `.updated` and the "completed" ones before the commit → deltas that extend what was relayed; the first `.completed` after `input_audio_buffer.committed` → the one `transcript.done` (no `content_index`: the caller's audio item has one part, index 0); repeats after it dropped |
| Charge | the ASR model's own usage on each `.completed`: `duration` → `input_transcription_milliseconds`, read to the microsecond and rounded up per turn | none: xAI's Voice Agent bills "duration and text events sent without audio", with no transcription line, so the session meter is unchanged |

**One transcript per item** (contract set 6.0.0). Whatever the provider sends,
each committed input item gets exactly one `transcript.done`, its final
transcript, after deltas that only append: the first completed transcript
after the item's commit is final, one before it is a running transcript, and
one after it is a repeat (`session.inputTranscript`). xAI sends a running
transcript, a "completed" one at each pause and the final one only after the
commit, and repeats it (measured 2026-10-16). An item whose final transcript
never comes is given the text it has when the next item is committed, before
that commit. A running transcript that rewrites what was relayed adds no delta
(the contract's delta only appends); the `transcript.done` carries the whole
text.

**Each item, its own words** (`@kaana.ai/contracts` 6.1.0; no parsed byte
changes, so the contract set stays 6.0.0). An item's deltas and its
`transcript.done` are the text of that item's audio, never of an earlier
item's. xAI's transcriber restarts when a response ends, not per item: when the
caller goes on talking while a response is generated, xAI commits what came
before as one item and the rest as another, and every transcript of the second
starts with the first one's words (measured on grok-voice-3.0, 2026-10-16;
replayed by `testdata/xai-interrupted-turn.jsonl` and, with a pause in which
the answer ends, `testdata/xai-ended-turn.jsonl`). On a dialect with
`continuesInterruptedTurns`, an item begun before any response.done since the
previous item carries that item; Kaana cuts its words from every transcript of
the new one, word by word ignoring case and punctuation (`inputItem.ownText`).
A transcript that no longer starts with them is relayed whole, so no word is
lost. OpenAI transcribes each committed item's own audio ("the transcription of
the audio in the item", server-events reference), so it carries nothing.

**Units.** OpenAI bills input transcription "according to the ASR model's
pricing rather than the realtime model's". Reporting that as
`audio_input_milliseconds` beside the session model's `audio_input_tokens`
would be the same audio read twice under the unit rule, and folding it into the
tokens would price it at the wrong model. Contract set 4.1.0 therefore adds
`input_transcription_milliseconds`: a second model's reading, a charge no other
unit counts (`money.ts`). It is `provider_reported`, like the rest of an
OpenAI session's units. A `tokens` usage (the 4o transcribers) is a charge that
unit does not express: the declared model is duration-billed, and a session
whose provider reports tokens for it ends (`upstream_error`) rather than settle
a guess. A transcript is relayed only once asked for, but a charge the provider
reports is settled either way. UNVERIFIED until the first signed canary: that
OpenAI's in-session usage for `gpt-transcribe` is `duration`, as it is for its
file transcriptions; that xAI's invoice shows no transcription line.

**Price.** A rate card for an `openai-realtime` deployment prices
`input_transcription_milliseconds` at `gpt-transcribe`'s rate: $0.0045 / 60 000
ms = `75000` in 10⁻¹² USD per ms. Oxy's price version for the route prices it
for the customer. Neither exists today (no `openai-realtime` rate card is
checked in; "Operating it").

**The signed-envelope lane refuses it.** `/internal/v1/realtime` is consumed
by the Oxy edge, which settles against a closed unit set that
has no `input_transcription_milliseconds`, so a report carrying it would be
one the edge cannot parse. `kaana.Executor.OpenSession` refuses
`config.inputAudioTranscription` at open (`invalid_request`, before anything is
dialled) and `internal/realtime` refuses a `session.update` carrying it without
acknowledging it (`kaana.EnvelopeInputTranscriptionRefused`). It is served on
`kaana.ai/v1`, whose grant and settlement speak 4.1.0; the lane and its
refusal are deleted together in ADR 0002 phase 5.

## What the catalogue says about a realtime model

The publisher writes the adapter's reviewed statement
(`providerconfig.RealtimeCapabilities`) into each session deployment's
`observed` block, as it does reasoning efforts, because neither provider's
model list says anything about sessions; `inventory.Parse` validates it and
`GET /internal/v1/models` serves the line's intersection across its routes
(`realtime`, `audio` on the catalogue entry). The public catalogue publishes
product capabilities only (Kaana ADR 0003): `capabilities.realtime
{transports, sessionKinds, inputAudioTranscription, audioFormats, defaultKinds}`, `audio
{voices, languages}` and `pricePerMinute`.

`defaultKinds` (contract set 8.16.0, ADR 0012 §4) is the one realtime field that is not the
inventory's: it is declared in `configs/public-catalogue.json`, a reviewed catalogue decision naming
the line a client opens for a kind. Each default kind is one of the line's `sessionKinds`, and at most
one active line is the default of a kind (`declaredCatalogueSchema`, checked by `validate.mjs` with
its controls, and by `publicapi.ParsePublicCatalogue` when Kaana loads the file). `GET /v1/models`
publishes a declared default only for a kind the inventory serves on the line, and only to a reader
whose contract set has `defaultKinds` (8.16.0 or later). `openai/gpt-live-1` is the default of `live`.

| | OpenAI | xAI |
|---|---|---|
| Transports, kinds | `websocket`; `conversation`, `live` | `websocket`; `conversation`, `live` |
| `inputAudioTranscription` | `true` | `true` |
| Encodings in and out | `pcm16_24khz`, `g711_ulaw`, `g711_alaw` | the same |
| Voices | the 10 documented (`alloy` … `marin`, `cedar`), no gender | the 26 built-in ids xAI documents; gender only for `eve` (female) and `rex` (male), Kaana's reviewed product voices |
| Languages | none: OpenAI documents no language list for its Realtime models | xAI's 20 documented codes; every voice speaks every one |

GPT-Live (`openai-live`) states `websocket` and `live` only, `inputAudioTranscription: true` (a
`live` session always relays the caller's words), the three encodings in and out, the 38 voices of its
reference with a gender only for `marin` (female) and `cedar` (male), the owner's reviewed product
voices (ADR 0012 §3), and no languages. Its internal statement names `server_vad` and `semantic_vad`,
each metered in `session_milliseconds` alone, and no output modality, tool choice or output cap: the
caller's backend owns the answer.

`gender` is never inferred from a name, and an empty language list is
published rather than guessed. A model's `languages` describe it; a session
names the client's own tag whatever the list says.

`pricePerMinute` is Kaana's price of one minute of a conversation under
`server_vad`, Kaana's default turn detection: the units that detection is
metered in (`MeteredUnits`) priced for a minute (60 000 ms of a clock or audio
unit; 600 audio input and 1200 audio output tokens for a token-priced model),
rounded up to twelve decimals. It is what a client reserves or shows; a session
settles what it measured.

### How Kaana serves every contract value: the internal statement

`contract.RealtimeParameters`, in each dialect's `providerconfig.RealtimeStatement`,
is Kaana's own table of what the provider's wire takes: its detections and
their threshold range, whether a detected turn's responses are fixed, the
units each detection is metered in, the output modality sets, the tool
choices, the output token cap, whether transcription takes a prompt, and
(`LanguageDefaults`) the region to send for a bare language. It is never
published. It drives:

- the translation: `NumberRange.Map` (the threshold), `WireTurnDetection`
  (`semantic_vad` → `server_vad`), `WireOutputModalities` (ask for audio when
  the wire cannot produce what was asked), the token cap clipped to the
  wire's, `NormalizeLanguage`, a prompt sent only where taken;
- the emulation: `toolChoice: none` as `tools: []`, a text-only response from
  its audio's transcript (`session.outputTranscript`), the audio of a response
  that did not ask for it measured and not relayed (`relaysAudio`);
- the refusal of the impossible, `RealtimeParameters.Unservable`, the one rule
  the public API applies before a grant (`checkRealtimeOpen`) and the dialect
  applies before writing (`refuseUnservable`): `invalid_request`, the
  contract's `param`, a message that names no provider.

| | OpenAI | xAI |
|---|---|---|
| `turnDetection` | `none`, `server_vad`, `semantic_vad`, each with its own `createResponse`/`interruptResponse` | `none`; `server_vad` with fixed responses (both `true`) and `threshold` 0.1–0.9; `semantic_vad` served as `server_vad` |
| Metered units | the token units and `input_transcription_milliseconds`, under every detection | `none`: audio in and out ms and `requests`; `server_vad`: `session_milliseconds` and `requests` |
| Output modalities on the wire | `["audio"]` or `["text"]` | `["audio"]` or `["audio","text"]` |
| Tool choices on the wire | `auto`, `none`, `required`, `function` | `auto` only (no field; `auto` is sent as nothing; `none` emulated for the session) |
| Output token cap | 4096 | none: not sent |
| Transcription prompt | taken | not taken: not sent |

`TestTheDialectTranslatesEveryContractValue` crosses every contract value with
both dialects: it is either written to the wire (as it is, translated or
emulated) or refused exactly as `Unservable` says, naming its field and never
the provider; nothing in between.

## Publishing

`configs/model-attribution.json` attributes `grok-voice-3.0` to
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
(`gpt-realtime-translate`) sessions, and `gpt-live-1` is a `live` session on its
own protocol. A session model is attached only to a slug whose adapter opens its
kind, so Realtime conversation models only under `openai-realtime` and
`gpt-live-1` only under `openai-live`
(`TestGPTLiveIsAttributedOnlyToTheLiveAdapter`). `TestOpenAIRealtimeIsAttributedOnlyToTheRealtimeAdapter`
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
  beside the text units) and `input_transcription_milliseconds` at
  `gpt-transcribe`'s rate (`75000`, "Input transcription"); without one,
  every attempt says its cost is unknown.
  OpenAI Realtime never reports `session_milliseconds`, so its card needs no
  rate for it (Oxy's price version still prices it, at zero).
- Sessions live in the memory of the task that opened them. Configured tasks
  relay a wrong-task resume once to its live owner; no state or admission moves.
  An unavailable owner is still refused (wire rule 7); the edge recovers the
  report by `requestId` once the original task settles. See
  [cookie-free peer resume](#cookie-free-peer-resume).
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
  reviewed id map in `.github/credential-admin-operations.json` and reads
  them back; a key id for a slug the publisher does not discover is ignored,
  so the map may name `xai-realtime` before the switch is applied.
- Price it: a rate card for each published `xai-realtime` deployment with
  `audio_input_milliseconds`, `audio_output_milliseconds`,
  `session_milliseconds` and `requests` (above); without one every session's
  operator cost is unknown. A rate card is keyed by deployment id, and a
  deployment id carries the date the publisher first observed the line
  (`dep_xai_realtime_grok_voice_3_0_observed_<date>`). It is
  `rc_xai_realtime_2026_10_16` in `configs/provider-rates.json`, for
  `dep_xai_realtime_grok_voice_3_0_observed_2026_10_16` — the line's first
  observation on the day the card ships, the same id
  `configs/deployment-commercial.json` sells (it replaced
  `rc_xai_realtime_2026_09_30`, whose 2.0 deployment xAI stopped serving) — read by the
  serving task through `KAANA_PROVIDER_RATES_PATH=/etc/kaana-rates/provider-rates.json`. Oxy's price
  version for the route must price `session_milliseconds` too (Oxy holds it for
  every realtime session and refuses a route that leaves it unpriced).
- xAI speaks whatever it is asked, so a text-only session is served from its
  audio's transcript (above). The signed canary is a spoken session
  (`outputModalities: ["audio"]`, `outputAudioFormat`, `voice`) that checks the
  transcript. A `server_vad` session needs `createResponse: true` and
  `interruptResponse: true`; its threshold is mapped onto xAI's range. Health
  stays `degraded` until a signed canary session has been validated.

### Operating `xai-stt`

xAI's streaming speech-to-text (Kaana ADR 0006) is a `transcription` session
under its own slug, `xai-stt`, served by `internal/provider/xaistt` with the
real-wire fake `xaistttest`. The model is `grok-voice-transcribe-2.0`, the
line `xai` serves as file transcription; `publicapi.resolveGrant` keeps, of a
line's routes, only those whose adapter performs the grant's operation
(`Registry.ServingKinds`), so a session never reaches the request adapter and
a file transcription never reaches the session adapter.

| Contract | xAI (`wss://api.x.ai/v1/stt`) |
|---|---|
| open | query parameters: `model`, `encoding`/`sample_rate` (`pcm16_24khz` → `pcm`/24000, `g711_ulaw` → `mulaw`/8000, `g711_alaw` → `alaw`/8000), `endpointing`, `vad_threshold`, `interim_results`, `language`; open once `transcript.created` arrives |
| `turnDetection: none` | `endpointing=5000`; the caller commits |
| `server_vad` | `endpointing = silenceDurationMs` (default 500), clamped to 0–5000; `threshold` → `vad_threshold`; `prefixPaddingMs` changes nothing a transcriber sends |
| `semantic_vad` | runs as `server_vad` with Kaana's default silence, as on every wire without semantic detection (ADR 0003) |
| `inputAudioTranscription.language` | xAI's spelling from the file transcription's language table, or none to detect; `prompt` is not sent (xAI takes none) |
| `input_audio.append` | the decoded audio as one binary frame |
| `input_audio.commit` | `{"type":"finalize"}` |
| any other command | refused, non-fatal (`type`); nothing is written |
| `transcript.partial is_final:false` | `transcript.interim` with the item's whole provisional text, only when `interimResults` was asked for |
| `is_final:true, speech_final:false` | `transcript.delta` with what the locked text adds |
| `speech_final:true` | the last delta (when the final extends the locked text), `input_audio.committed`, and the item's one `transcript.done` with xAI's final text |
| `error` | fatal, `provider_error`, message redacted; xAI documents no error type |

Item ids are Kaana's, `item_<n>` per utterance. `session.close` ends the
session at once, so a caller that wants the last utterance commits it and
waits for its `transcript.done` first.

**Metering.** `session_milliseconds`, `oxy_measured`: the wall clock from
`transcript.created` to the end of the connection, rounded up once. xAI prices
streaming at $0.20 an hour without saying whether audio or connection time is
counted; the wall clock bounds either. The line's file transcription settles
in `audio_input_milliseconds`, so the one declared price of the line prices
each mode in its own unit.

**Serving it**, in order:

1. Import the key from stdin under the slug, with the reviewed key id
   `.github/credential-admin-operations.json` names for discovery
   (`eadb194f-def3-4b3b-8a56-2de50a6f5e05`): `kaana-platform-credential-import --operation-id
   kpc_<32 hex> --provider xai-stt --key-id eadb194f-def3-4b3b-8a56-2de50a6f5e05 --class paid
   --position 1 --actor operator:<id> --signing-key-id <id> --signing-key-file
   <file> --endpoint https://<internal-host>/internal/v1/platform-provider-credentials/mutations
   < xai-key.txt` ("Container and deployment" in operating.md). It may be the
   `xai-realtime` secret (same team and credit) or, preferably, a second key on
   the same team.
2. oxy-infra's switch adds `xai-stt` to the serving task's `KAANA_PROVIDERS`
   (with `KAANA_PROVIDER_XAI_STT_PROTOCOL=xai_stt`) and to the publisher's
   `KAANA_DISCOVERY_PROVIDERS`; the deploy workflow already writes
   `KAANA_PROVIDER_XAI_STT_DISCOVERY_KEY_ID`. The process refuses to start on a
   slug with no enabled credential, so step 1 comes first.
3. Read back the deployment id the publisher mints
   (`dep_xai_stt_grok_voice_transcribe_2_0_observed_<date>`), then, in one
   reviewed commit: a rate card pricing `session_milliseconds` at 55555
   (10⁻¹² USD per millisecond, $0.20 an hour rounded down), the
   `configs/deployment-commercial.json` entry, and the line's declared
   `capabilities.realtime` (`websocket`, `transcription`) with a new price
   version adding `session_milliseconds` at $0.20 an hour and
   `audio_output_milliseconds` at zero (Oxy quotes a session minute over every
   duration unit).
4. A signed canary session (below). Health stays `degraded` until then.

**Canary.** Open `GET /v1/realtime?model=x-ai/grok-voice-transcribe-2.0` with
`kind: transcription`, `inputAudioFormat: pcm16_24khz`, `server_vad`,
`interimResults: true`; stream 60 s of recorded speech in real time; commit,
wait for `transcript.done`, close. Check: `transcript.interim` and
`transcript.delta` arrived, the final text matches the recording, the report
is `session_milliseconds` ≈ 60–65 s, and xAI's console shows the session's
charge within the wall-clock figure.

### Operating `openai-live`

- Serve it: add `openai-live` to the serving task's `KAANA_PROVIDERS`. Protocol and root are locked;
  a `_BASE_URL` other than `https://api.openai.com/v1`, or `openai_live` under another slug, is refused
  at startup. A `live` session is served on kaana.ai/v1 only.
- Its key: a key row belongs to exactly one slug, so the OpenAI key is imported once more under
  `openai-live`, from stdin, with the reviewed key id `.github/credential-admin-operations.json` names
  for discovery, `ac291e20-2c8e-4036-8bad-0e956f9af69f` (the row exists since 2026-11-14, the same
  OpenAI project as the `openai` row; ADR 0012 §9.1). The account must be on a paid tier (GPT-Live does
  not serve the free tier). With exactly one key the provider default serves every deployment; bind
  explicitly with `kaana-credentials bind-deployment --provider openai-live` once there are two.
- Order: the credential row exists before the switch, because the serving process refuses a
  `KAANA_PROVIDERS` slug with no enabled credential and the publisher one whose discovery key id names
  no enabled row. The switch is oxy-infra's `kaana_openai_live_enabled` (OxyHQ/oxy-infra#464), which
  adds `openai-live` to the serving task's `KAANA_PROVIDERS` and the publisher's
  `KAANA_DISCOVERY_PROVIDERS`; the deploy workflow already writes
  `KAANA_PROVIDER_OPENAI_LIVE_DISCOVERY_KEY_ID` from the reviewed id map and reads it back, and a key id
  for a slug the publisher does not discover is ignored.
- Price it, once the publisher has minted the deployment
  (`dep_openai_live_gpt_live_1_observed_<date>`, the date it first observed the line): in one reviewed
  commit, a rate card pricing `session_milliseconds` at `833334` (10⁻¹² USD per ms: $0.05 / 60 000 ms,
  rounded up, ADR 0012 §5) and the `configs/deployment-commercial.json` entry for that id, read by the
  serving task through `KAANA_PROVIDER_RATES_PATH`. Without the card every session's operator cost is
  unknown.
- Publish it: the `openai/gpt-live-1` entry of `configs/public-catalogue.json` declares
  `capabilities.realtime.defaultKinds: ["live"]` and its price version, which prices
  `session_milliseconds` for the customer: `01a1d9d6-cdbb-79aa-96ee-335708a286fe`, $0.05 per 60 000 ms
  and nothing else, which Oxy's clock-only session plan requires. Oxy reads the declared catalogue at
  contract set 8.16.0 (oxy#2288), and `GET /v1/models` renders `live` and `defaultKinds` only for a
  reader of that set or later (Kaana#321). The line appears there once the inventory serves it.
- Health never opens a session and stays `degraded` until a signed canary session has been validated
  (ADR 0012 phase K5): a `live` session on the catalogue default, a spoken turn, a delegation answered
  with a verbatim span, and a close whose `session_milliseconds` matches OpenAI's usage for it.

## Findings, refusals and what is not verified

The findings, the refusals by name and the list of what is not verified are a dated record, kept in
[realtime-findings.md](realtime-findings.md).

## Cookie-free peer resume

[ADR 0005](adr/0005-cookie-free-realtime-resume.md) replaces load-balancer
stickiness with a private one-hop relay. This is placement, not another
identity or grant. Public and signed internal session commands remain unchanged.

Migration `0023` adds `realtime_session_owners`: request ID, lane, private task
IPv4, random process generation and a 45-second lease. The runtime role may
execute four bounded functions, with no direct table access. A session claims
its location before opening the provider or sending `session.created`, renews
it every 15 seconds, and removes it on completion. Failed renewal stops the
session; CAS prevents an old process from renewing or deleting a replacement.
The lease does not extend the session's resume window, duration or authority.
Expired crashed-owner rows are removed in bounded batches on subsequent claims.

`KAANA_REALTIME_PEER_VPC_CIDR` is the exact VPC CIDR discovered from all live
service subnets by the deployment workflow. The task obtains its own address
from its ECS container metadata (`kaana`, `awsvpc`, one private IPv4). Managed
startup refuses missing or inconsistent placement. Local development without
ECS metadata keeps the existing single-process mode. No public URL, customer
header or arbitrary endpoint can supply peer placement; metadata and private
connections bypass environment HTTP proxies.

The private `GET /internal/peer/realtime-resume` accepts direct VPC connections
addressed to this task's IP on port 8080. It rejects `Forwarded`, every
`X-Forwarded-*`, and `CF-Connecting-IP` header before upgrade, keeping ALB
requests out. The owner sends only its opaque process generation; the caller
compares it with the lease before forwarding the original credential or signed
resume frame. The owner rechecks its live directory entry and original auth.
The endpoint only resumes a local session: it cannot admit, open, or forward.
Public resume retains the original credential digest; internal resume retains
signature verification. The public gate only extracts the presented digest on
resume; the owning session compares it with the digest admitted at creation.
The generation check relies on the server-owned directory and private network;
it is a stale-address guard, not a new cryptographic peer identity protocol. Public origin policy still runs at the public entry.

The relay bounds handshake, frame size and each write, pings the public
connection, and closes both sockets on cancellation or local shutdown. Draining
an intermediate task leaves the owner's session resumable within its original
window. Commands, audio, credentials and transcripts never enter the directory
or logs. Provider execution, replay numbering, deduplication and exactly-once
settlement stay on the owner.

### Rollout and rollback

Source preparation is not evidence that production cookies are disabled.
Deploy the narrow infrastructure change first: serving-task self ingress and
egress on TCP 8080, the VPC environment value, and deployment-role read access
to `DescribeSubnets`/`DescribeVpcs`. Keep existing public access and target-group
cookie settings during this compatibility stage.

The normal AWS workflow applies additive migration `0023` using the migrator
role before moving any service. It changes no existing schema or settlement
records; the previous image ignores the new table and remains rollback-compatible.
Verify the new serving digest on every task, public and internal wrong-task
resume, one provider admission/settlement, wrong credentials, and owner loss.
Before removing stickiness, prove either that the maximum pre-change session
lifetime (one hour), plus resume/lease grace, has elapsed since old-session
admission stopped, or that every old serving task is STOPPED and no old owner
remains alive in the verified fleet/directory. A stopped task cannot retain its
in-memory provider connection; no extra one-hour wait is required after that
proof. Verify every remaining owner runs the exact new image. Alia has its own
voice-session drain; its receipt does not replace Kaana's evidence.

Remove target-group cookie settings only in the separately reviewed infra
change after those gates. After that change, rolling back to an image without
peer resume requires restoring routing compatibility before accepting new
sessions; otherwise wrong-task reconnects will refuse. Preserve receipts and
use the existing session refusal/report recovery if an owner is lost; do not
replay admission or fabricate an empty settlement.

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
- **Input transcription is the deployment's declared model, billed under its
  own unit or not at all**: `input_transcription_milliseconds` where the
  provider bills the ASR model apart, nothing where the session price
  includes it, and a session ends on a transcription charge that unit cannot
  express. A transcript is relayed only once asked for; a reported charge is
  always settled. The realtime statement the publisher writes is the one the
  dialect reads (`TestTheCatalogueStatementIsWhatTheAdapterServes`).
- **No provider variant reaches a client** (Kaana ADR 0003): every value the
  contract offers is translated or emulated, and only what `Unservable` says
  is impossible is refused, with the contract's `param` and a message that
  names no provider. Nothing of a dialect's statement is published.
- **A provider that reports the session's usage once, at its end, settles through
  `provider.RealtimeFinalUsage`**: read once at settlement, `provider_reported` when the provider
  confirmed it, Kaana's wall clock `oxy_measured` when it never came, and never a cumulative snapshot
  summed (GPT-Live's `session.usage.updated`).
- **Every adapter that serves `live` passes `TestLiveContract`**, on its own real-wire fake.
- **A `live` session's delegation state is the session's, never an adapter's**: the session
  supersedes, refuses a command for any delegation but the open one, and refuses the commands a
  `live` session does not take before they are acknowledged. An adapter returns
  `delegation.requested` and serves or refuses what `Send` hands it.
- **An emulated `live` session relays only what a native one can**: transcripts, the voice's
  audio, errors and `delegation.requested`; the conversation it runs on (responses, items, tool
  calls, conversation commands) never reaches the client, its units always reach settlement, and the
  preamble and verbatim marks are `internal/provider/live.go`'s, shared with the native adapter.
- **A provider speaking the OpenAI Realtime shape is a dialect of
  `internal/provider/openairealtime`, not a copy**: its differences are
  `dialect` fields, each from the provider's documentation, and it gets its own
  slug, origin and fake (`openairealtimetest.NewXAI`).
