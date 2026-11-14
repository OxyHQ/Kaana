# ADR 0012 — Live voice sessions with delegation

- Status: accepted (2026-11-14), with the owner's answers to the first draft's open points (§9). Being
  built in the phases of §8. Production is unchanged until phase K5 and its canary have run.
- Authority: the owner's decisions of 2026-11-14, relayed by the coordinator: voice mode uses OpenAI
  GPT-Live (`gpt-live-1`) instead of xAI Grok Voice. Switching later to Grok voice, ElevenLabs or any
  other voice provider must be a catalogue or routing change, not a rewrite. The voice front end hands
  every user turn to the client's own backend ("client delegation"), so a call has exactly the tools,
  memory and model of a typed turn. The owner approved the contract change explicitly ("ok pues mejor el
  live si… que sea fácil cambiar").
- Date: 2026-11-14
- Consumers: Gemies voice mode (Gemies ADR 0041, which amends Gemies ADR 0017), and through it the Alia
  and Gemies apps.

## Context

Voice mode today is a `conversation` realtime session (`docs/realtime.md`): the speech-to-speech model
is the brain. Gemies hands it the gemie's instructions and a bridged copy of its tools
(`packages/api/src/lib/voice/tool-bridge.ts` in Gemies), and the model calls them. Production serves one
line, `x-ai/grok-voice-3.0` under `xai-realtime`. `openai-realtime` is built
(`internal/provider/openairealtime`) but has no key, no deployment and no rate card.

That design has two costs. A call is a second brain, with a voice-only tool list, its own instruction
budget and a model that is not the one the person chose for text. And each provider is chosen together
with its reasoning: changing the voice means changing who answers.

OpenAI documents GPT-Live as a voice front end that delegates the reasoning
(https://developers.openai.com/api/docs/guides/live-conversations,
https://developers.openai.com/api/docs/guides/live-delegation,
https://developers.openai.com/api/reference/resources/live/primary-websocket,
https://developers.openai.com/api/docs/models/gpt-live-1, all read 2026-11-14):

- `wss://api.openai.com/v1/live/sessions`, `Authorization: Bearer`, no query parameters. The first frame
  is `session.start {session: {model, instructions?, input?, audio: {format, output: {voice}},
  delegation, store?}}`, answered by `session.started`. `model`, `instructions` (16 384 tokens),
  `input` (up to 128 text items), `audio` and the delegation type are immutable after start.
- Audio is full duplex, one format both ways: `audio/pcm` at 24 000 or 16 000 Hz, `audio/pcmu` or
  `audio/pcma` at 8 000 Hz. In: `session.input_audio.append {audio}`, not acknowledged; `mute`/`unmute`.
  Out: `session.output_audio.delta {delta}`, with no done event and no timing. GPT-Live decides when to
  listen and when to speak. No turn-detection or barge-in setting is documented.
- 38 built-in voices (`alloy` … `willow`, default `marin`) or a custom voice `{id}`. No gender is
  documented for any of them.
- `delegation: {type: "client", acknowledgement_delay_ms: 0..800}`. `session.delegation.created
  {delegation: {id, target: "client"}}` hands a turn to the client. It carries no words: the client
  rebuilds the request from `session.input_transcript.delta` and `session.output_transcript.delta`
  (`delta`, `start_ms`, `end_ms`). The client answers with `session.commentary.append {delegation_id,
  content}`, which is spoken (paraphrased), with `session.thinking.append`, which is silent context (an
  empty one suppresses the automatic acknowledgement), or with `session.instructions.append`. Each append
  holds at most 500 tokens and is acknowledged by `*.appended {client_event_id}`. No event ends or
  cancels a delegation. A person talking over the answer does not stop the client's backend work.
- `session.usage.updated {usage: {seconds}}` is cumulative. `session.closed {reason: close_requested |
  expired | content | remote_hangup | connection_lost, usage: {seconds}}` is final. A socket that closes
  before `session.closed` leaves the usage unconfirmed. `error {error: {type, code, message, param,
  client_event_id}}`.
- Price: $0.05 a minute, billed per second, never rounded up to the minute. Backend work is billed apart.
  Concurrent sessions: Build 50, Launch 300, Grow 500. The free tier is unsupported. No maximum duration
  is published, though the `expired` reason exists.

## Decision

### 1. One canonical live session kind: `live`

`realtimeSessionKindSchema` gains `live`: a spoken conversation whose turns the caller's backend answers.
Voice mode opens only this kind. `conversation` stays as it is, for callers that want the model to answer
by itself.

**Configuration** (`realtimeSessionConfigSchema`, cross-field rules in `realtimeConfigIssues`). A `live`
session:

| Field | Rule for `live` |
|---|---|
| `instructions` | The persona only: who speaks, its tone, the language. Never the backend's instructions or tools |
| `voice` | Required. An id from the line's `audio.voices` |
| `inputAudioFormat`, `outputAudioFormat` | Both required and equal: one format both ways (GPT-Live's rule; the emulated wires accept it too) |
| `turnDetection` | `server_vad` or `semantic_vad`. The voice decides when a turn ends; `none` is refused. Thresholds and silence are translated where the wire takes them and ignored where the voice owns turn-taking (GPT-Live) |
| `inputAudioTranscription` | Optional, `{language?}`. The caller's words are relayed as `transcript.*` with `source: input_audio` whether or not it is set: delegation needs them. `prompt` and `interimResults` are refused |
| `delegation` | Required for `live`, refused for every other kind: `{acknowledgement: 'auto' \| 'none'}`. `auto` lets the voice say a short "one moment" while the backend works |
| `outputModalities`, `tools`, `toolChoice`, `maxOutputTokens`, `translation` | Refused: the backend owns the answer and its tools |

`session.update` cannot change a `live` session (GPT-Live fixes everything at start): it is refused with
`invalid_request`, `param: type`.

**Commands** (client → Kaana), with the usual `schemaVersion`, `requestId`, `commandId`:

| Command | Fields | Meaning |
|---|---|---|
| `input_audio.append` | `data` | As today. `input_audio.commit` and `.clear` are refused for `live`: the voice detects turns |
| `delegation.reply` | `delegationId`, `text` (1–8 192 UTF-16 units), `verbatim?` (≤ 32 strings, each an exact substring of `text`, 1–256 units), `final` | Speak this part of the answer. A client streams sentence by sentence; Kaana splits to the provider's bound. `verbatim` marks spans the voice must say exactly: numbers, addresses, names, codes |
| `delegation.context` | `delegationId`, `text` (1–8 192) | Silent context the voice may use, for example "still checking your calendar" |
| `delegation.cancel` | `delegationId` | The client dropped the turn: nothing more is spoken for it |
| `session.close`, `session.resume` | | As today |

The voice is told, in a fixed preamble Kaana adds before the caller's persona, that every request is
answered by delegation and that a span between `⟦` and `⟧` is spoken exactly as written. Kaana wraps
each `verbatim` span in those marks on the wire and never relays them back in `transcript.*`.

**Events** (Kaana → client), numbered as every realtime event is:

| Event | Fields | Meaning |
|---|---|---|
| `delegation.requested` | `delegationId` (1–128), `text` | One turn to answer. `text` is the caller's words since the previous delegation, assembled by Kaana from the input transcript, so no client stitches transcripts |
| `delegation.superseded` | `delegationId` | The person moved on (a newer delegation opened before this one's `final` reply). Its reply is no longer spoken |
| `transcript.delta`, `transcript.done` | as today | `source: input_audio` (the person) and `output_audio` (the voice) |
| `output_audio.delta` | as today | Kaana numbers frames; GPT-Live has no done event, so `output_audio.done` is not sent for `live` |
| `session.created`, `session.closed`, `error`, `command.accepted` | as today | |

A `delegation.*` command naming a delegation that is not the open one (unknown, superseded, cancelled
or already final) is refused with a non-fatal `invalid_request` naming `delegationId`, before it is
acknowledged; a repeated `commandId` is still answered `duplicate: true`. The open delegation is the
newest, until its `final` reply or its `cancel`. The three `delegation.*` commands on any other kind are
refused with `invalid_request`, `param: type`, like `session.update` on a `live` session
(`realtimeSessionTakes` in the contract). The session, not the adapter, emits `delegation.superseded`
when a newer delegation opens before the open one's `final` reply, so every adapter of §2 shares that
rule and only returns `delegation.requested`.

(K1, 2026-11-14: the verbatim span's lower bound of 1 unit, the refusal of `context` and `cancel` as
well as `reply`, and the session as the one place that supersedes were settled while building the
contract; none changes what a client sends or reads for a valid turn.)

**Catalogue.** A line that serves `live` lists it in `capabilities.realtime.sessionKinds`.
`capabilities.realtime.defaultKinds` (new, optional) lists the session kinds for which the line is
Kaana's default; at most one active line is the default of a kind, which `configs/public-catalogue.json`
validation enforces.

### 2. Provider adapters stay in Kaana; delegation is native or emulated

`provider.RealtimeAdapter` serves `live` for any provider that can speak a turn it was handed:

- **Native: OpenAI GPT-Live.** New slug `openai-live`, protocol `openai_live`, package
  `internal/provider/openailive`, real-wire fake `openailivetest`. `session.start` carries the model, the
  preamble plus persona as `instructions`, `audio.format`, `audio.output.voice` and
  `delegation: {type: "client", acknowledgement_delay_ms}` (`auto` → 400, `none` → 0). Then:
  `session.delegation.created` → `delegation.requested` with the input transcript accumulated since the
  previous delegation; `delegation.reply` → `session.commentary.append`, cut at 500 tokens on sentence
  boundaries, verbatim spans never cut; `delegation.context` → `session.thinking.append`;
  `acknowledgement: none` → an empty `session.thinking.append` on each delegation;
  `delegation.cancel` → `session.instructions.append` naming the turn as dropped;
  `session.input_transcript.delta` / `session.output_transcript.delta` → `transcript.delta`, with
  `transcript.done` at each delegation (input) and at a gap of 1.5 s in output deltas (output);
  `session.output_audio.delta` → `output_audio.delta`; `session.usage.updated` / `session.closed`
  `usage.seconds` → `session_milliseconds`, `provider_reported`. A socket that ends without
  `session.closed` settles Kaana's own wall clock, `oxy_measured`.
- **Emulated: the OpenAI-Realtime-shaped wires** (`openai-realtime` `gpt-realtime-*`, `xai-realtime`
  `grok-voice-*`), and later any speech-to-speech provider without delegation. The dialect configures one
  canonical tool, `delegate {request: string}`, `tool_choice: auto` (sent as nothing on xAI), audio output,
  input transcription on, and the same preamble. The model's completed `tool_call` for `delegate` becomes
  `delegation.requested` (`delegationId` = the call id; `text` = the input transcript since the previous
  delegation, or the model's `request` when no transcript arrived). Replies are buffered until `final`,
  then sent as the call's `function_call_output` followed by `response.create`; `delegation.context`
  received before `final` is prepended to that output. A new delegation before `final` supersedes the
  old one. The dialect's statement still decides voices, formats and billing (`clockBilled`,
  `tokenUsage`).

A client never sees which of the two is serving. One suite in `internal/realtime`, `TestLiveContract`,
runs the whole canonical contract (open, audio both ways, transcripts, a delegation answered in two
replies with a verbatim span, supersession, cancel, refusals, close and settlement) against
`openailivetest` and against `openairealtimetest` in both dialects. An adapter that does not pass it is
not served for `live`.

### 3. Voices are Kaana's, through the catalogue

`audio.voices` of a `live` line lists the provider's voice ids. Product gender is published only for the
voices Kaana has reviewed as product voices (`providerconfig`), never inferred from a name: for
`gpt-live-1`, `marin` is female and `cedar` is male (the owner's decision, 2026-11-14); for xAI, `eve`
and `rex`, as today. A client picks a voice by gender from the catalogue and never writes a voice id.

### 4. Selection is a catalogue default, never a client constant

`configs/public-catalogue.json` declares `capabilities.realtime.defaultKinds: ["live"]` on exactly one
active `live` line: `openai/gpt-live-1`. A client opens `GET /v1/realtime?model=<that line>` after reading
`GET /v1/models`, so the line is named by the catalogue, never by the client's code. Oxy (or Kaana's
native admission) still authorizes the session for the exact line like any other. Switching provider is
then one reviewed change of `defaultKinds` in the catalogue. Grok stays as the second `live` line and the
fallback: a client tries the default first and the next `live` line only on the model's own refusal, as
voice mode does today.

### 5. Billing

- GPT-Live is billed `session_milliseconds`, per second. The rate card holds OpenAI's figure for operator
  cost ($0.05 / 60 000 ms = `833334` in 10⁻¹² USD per ms, rounded up), and the line's Kaana price version
  prices the unit for the customer. `pricePerMinute` is that price for one minute.
- Oxy holds a `live` session with a duration-only ceiling: `session_milliseconds` ≤ the signed
  `maxDurationMs` plus the bounded open, and `requests: 1`. A line priced only in `session_milliseconds`
  quotes that plan; the token and audio-byte plans do not apply to it.
- The backend's turns are ordinary inference requests from the client, billed as such. Kaana never folds
  them into the session.
- An emulated provider is billed as its dialect bills today (xAI `server_vad`: `session_milliseconds`
  and `requests`; OpenAI Realtime: tokens), held by the plans Oxy already has.

### 6. Grok Voice

`xai-realtime` stays, converted to the `live` shape by emulation (§2), as the second adapter: it proves
the contract does not depend on one provider, and it is the fallback (§4). Its `conversation` kind stays
for callers that use it. xAI streaming dictation (`xai-stt`, `grok-voice-transcribe-2.0`) is a different
feature and is unaffected.

### 7. What does not change

The edge wire, resume, peer relay, limits, exactly-once settlement and key custody of `docs/realtime.md`
apply to `live` unchanged. A `live` session is one request for attribution and settles once.

## 8. Phases

| Phase | Repo | What | Done when |
|---|---|---|---|
| K1 | Kaana | `@kaana.ai/contracts` minor: kind `live`, `delegation` config, the three commands and two events, `defaultKinds`; Go mirrors; descriptor and OpenAPI regenerated; SDK types | `make check` green, packages released |
| K2 | Kaana | `openailive` adapter and fake, slug `openai-live`, attribution of `gpt-live-1`, discovery, voice statement, catalogue entry with `defaultKinds: ["live"]` and its price version | `TestLiveContract` passes against `openailivetest` |
| K3 | Kaana | Emulated delegation in `openairealtime` (both dialects); `live` added to their statements | `TestLiveContract` passes against both fakes |
| K4 | Oxy | Grants and ceilings accept kind `live`; the duration-only plan; contracts bump | Oxy CI green, deployed |
| K5 | Kaana, oxy-infra | Key under `openai-live` (§9.1), `kaana_openai_live_enabled`, rate card and `deployment-commercial.json` for the minted deployment id, deploy, signed canary | Canary receipt read back |
| G1 | Gemies | Voice mode opens `live` on the catalogue default, answers every `delegation.requested` with the chat route's own turn (Gemies ADR 0041); the tool bridge and the price ranking are deleted | Gemies CI green, production canary passes |

## 9. The owner's answers (2026-11-14)

1. **Key.** The same OpenAI project as the existing `openai` key row (`610adcc8-…`). A key row belongs to
   one slug, so `openai-live` gets its own row holding the same secret, imported through Kaana's own
   credential tooling without the secret ever being printed. The account must be on a paid tier.
2. **Paraphrase.** Accepted, with the verbatim spans of §1: the backend marks numbers, addresses, names
   and codes, and the preamble tells the voice to say them exactly. GPT-Live documents no verbatim mode
   (its `session.output_audio.fixture.play` plays prerecorded audio, not text).
3. **Voices.** `marin` is female and `cedar` is male, through the catalogue (§3).
4. **Grok.** Kept as the second `live` adapter and the fallback (§6).

## Consequences

- A call has one brain, the same one as text. Voice gets every tool, connector and gemie that text has,
  with no copy to keep in step.
- A spoken answer waits for a backend turn. The acknowledgement covers the gap, and the client streams
  its answer in chunks so the voice starts on the first sentence.
- Changing the voice provider is a catalogue default in Oxy, proven safe by `TestLiveContract`.
- The provider's raw delegation, transcript stitching and append limits never reach a client.
