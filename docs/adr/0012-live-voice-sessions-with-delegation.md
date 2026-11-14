# ADR 0012 — Live voice sessions with delegation

- Status: accepted (2026-11-14). Implementation is pending, in the phases of §8. Production is unchanged
  until phase K5 and its canary have run.
- Authority: the owner's decisions of 2026-11-14, relayed by the coordinator: voice mode uses OpenAI
  GPT-Live (`gpt-live-1`) instead of xAI Grok Voice. Switching later to Grok voice, ElevenLabs or any
  other voice provider must be a catalogue or routing change, not a rewrite. The voice front end hands
  every user turn to the client's own backend ("client delegation"), so a call has exactly the tools,
  memory and model of a typed turn.
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

`realtimeSessionKindSchema` gains `live`: a spoken conversation whose turns are answered by the caller's
backend. It is the only kind voice mode opens. `conversation` stays for callers that want the model to
answer by itself. Its model list is what Kaana serves today and nothing changes for it.

A `live` session is configured at open, and only at open:

| Field | Meaning |
|---|---|
| `voice` | A voice id from the line's catalogue `audio.voices` |
| `audioFormat` | One format both ways: `pcm16_24khz`, `pcm16_16khz`, `g711_ulaw`, `g711_alaw`. The line's `realtime.audioFormats` say which it takes |
| `persona` | How the voice speaks (name, tone, language). Never the backend's instructions or tools. Bounded by the contract |
| `language` | The caller's BCP 47 tag, as for input transcription |
| `history` | Prior turns as text, `{role: user \| assistant, text}`, bounded |
| `delegation.acknowledgement` | `auto`: the voice may say a short "one moment". `none`: it stays silent until the answer |

The commands and events, numbered and acknowledged as every realtime session's are (`docs/realtime.md`,
wire rules 5–10):

| Direction | Name | Meaning |
|---|---|---|
| client → Kaana | `input_audio.append`, `input_audio.mute`, `input_audio.unmute` | Audio and the microphone |
| client → Kaana | `delegation.reply {delegationId, text, final}` | Speak this part of the answer. Chunked by the client; Kaana splits it to the provider's bound |
| client → Kaana | `delegation.context {delegationId, text}` | Silent context the voice may use later (for example "still working on it") |
| client → Kaana | `delegation.cancel {delegationId}` | The client dropped the turn; the voice says nothing more for it |
| client → Kaana | `session.close` | As today |
| Kaana → client | `delegation.requested {delegationId, text, transcriptItemIds}` | One user turn to answer. Kaana assembles `text` from the input transcript since the previous delegation, so no client rebuilds it |
| Kaana → client | `delegation.superseded {delegationId}` | The person changed the request. Discard that turn's result |
| Kaana → client | `transcript.delta` / `transcript.done`, `source: input_audio \| output_audio` | As today |
| Kaana → client | `output_audio.delta` | As today. Kaana numbers the frames; there is no provider done event to relay |
| Kaana → client | `usage.updated {sessionMilliseconds}` | Running total, from the provider's own figure where it reports one |
| Kaana → client | `session.created`, `session.closed`, `error` | As today |

Turn-taking, barge-in and acknowledgement speech belong to the voice. A `live` session takes no
`turnDetection`, no `tools`, no `toolChoice` and no `outputModalities`, and the contract refuses them for
the kind (`realtimeConfigIssues`). The backend's tools are the client's own business.

### 2. Provider adapters stay in Kaana; delegation is native or emulated

`provider.RealtimeAdapter` serves `live` for any provider that can speak a turn it was handed:

- **Native** (OpenAI GPT-Live, new slug `openai-live`, `internal/provider/openailive`, real-wire fake
  `openailivetest`). `delegation.requested` comes from `session.delegation.created` plus the input
  transcript Kaana has been relaying. `delegation.reply` becomes `session.commentary.append`, cut at 500
  tokens on sentence boundaries. `delegation.context` becomes `session.thinking.append`.
  `acknowledgement: none` becomes an empty `thinking.append` on each delegation. `delegation.cancel`
  becomes `session.instructions.append` naming the turn as dropped. `usage.seconds` becomes
  `session_milliseconds`, labelled `provider_reported`, and a socket that ends without `session.closed`
  settles Kaana's own wall clock, labelled `oxy_measured`.
- **Emulated**, for a speech-to-speech provider without delegation (`openai-realtime` `gpt-realtime-*`,
  `xai-realtime` `grok-voice-*`, and later ElevenLabs Conversational AI). Kaana configures one canonical
  tool, `delegate {request: string}`, plus a fixed, reviewed persona preamble telling the model to call
  it for every request and to speak only what the tool returns. The model's `tool_call` becomes
  `delegation.requested` (`delegationId` = the call id, `text` = the input transcript, with the model's
  `request` kept as a hint). `delegation.reply` with `final: true` becomes the function output plus
  `response.create`. Barge-in maps to `delegation.superseded`. The dialect's existing statement still
  decides voices, formats and billing (`clockBilled`, `tokenUsage`).

A client never sees which of the two is serving. One test suite in `internal/realtime`
(`TestLiveContract`) runs the whole canonical contract against `openailivetest` and against the
`openairealtimetest` fakes in both dialects. An adapter that does not pass it is not served for `live`.

### 3. Voices are Kaana's, through the catalogue

`audio.voices` on each `live` line lists the provider's voice ids. Product gender is published only for
the voices Kaana has reviewed as product voices (`providerconfig`, as `eve` and `rex` are for xAI today),
never inferred from a name. For `gpt-live-1` the proposal is `marin` (female) and `cedar` (male), OpenAI's
documented default and its recommended pair on the Realtime models. The owner confirms it (§9). A client
picks by gender from the catalogue; it never writes a voice id.

### 4. Selection is a catalogue default, never a client constant

A `live` open may name `model: "default"`, a routing slug with no `/`, as a power level is. Oxy resolves
it from the application's policy (`liveVoiceDefault`, set per application in Oxy's catalogue) to signed
exact routes, as it resolves a power level for text. The catalogue marks the line that is the default
for the caller (`defaults.live`). Switching provider is then one Oxy catalogue change: point the default
at another `live` line. Gemies never names a voice model.

`modelReferenceSchema` accepts the slug only on a `live` open; every other open keeps the exact reference.

### 5. Billing

- The voice is `session_milliseconds`, per second, priced by the line's Kaana price version
  ($0.05 / 60 000 ms = `833333.33…` ⇒ `833334` in 10⁻¹² USD per ms, rounded up for the customer;
  the rate card holds OpenAI's exact figure for operator cost). `pricePerMinute` on the catalogue entry
  is that price for one minute.
- Oxy holds the session against `realtimeDurationCeiling` (`inferenceEdge.service.ts`) with the
  duration rule: the signed `maxDurationMs` × the line's `session_milliseconds` price. A `live` session
  has no audio-token units, so the token and byte rules do not apply to it.
- The backend's turns are ordinary Kaana inference requests from the client, billed as such. Kaana does
  not fold them into the session.
- An emulated provider is billed as its dialect bills today (xAI `server_vad`: `session_milliseconds`
  and `requests`; OpenAI Realtime: tokens). The client's price check reads `pricePerMinute` alone.

### 6. Grok Voice

`xai-realtime` stays only if it passes `TestLiveContract` through emulation, as a second adapter that
proves the contract is not shaped around one provider. It is then a catalogue alternative, not the
default. If it does not pass cleanly, it is deleted with its publisher probe, fake, rate card,
attribution and infrastructure switch, in one change. xAI streaming dictation (`xai-stt`,
`grok-voice-transcribe-2.0`) is a different feature and is unaffected.

### 7. What does not change

The edge wire, resume, peer relay, limits, exactly-once settlement and key custody of `docs/realtime.md`
apply to `live` unchanged. A `live` session is one request for attribution and settles once.

## 8. Phases

| Phase | Repo | What | Done when |
|---|---|---|---|
| K1 | Kaana | `@kaana.ai/contracts`: kind `live`, its config, commands and events, `defaults.live`, the `default` slug on a `live` open; descriptor and OpenAPI regenerated | `make check` green, contract published |
| K2 | Kaana | `openailive` adapter, fake, `providerconfig` slug `openai-live`, attribution of `gpt-live-1`, discovery through `GET /v1/models`, voice statement | `TestLiveContract` passes against `openailivetest` |
| K3 | Kaana | Emulated delegation in `openairealtime` (both dialects), or deletion of `xai-realtime` per §6 | `TestLiveContract` passes against both fakes, or the deletion lands |
| K4 | Oxy | `openai-live` provider row, price version (`session_milliseconds`), `liveVoiceDefault` for the Gemies and Alia applications, duration ceiling covered by tests | Oxy CI green, deployed |
| K5 | Kaana, oxy-infra | Key imported under `openai-live` (§9), `kaana_openai_live_enabled`, rate card and `deployment-commercial.json` for the minted deployment id, deploy, signed canary: a `live` session with audio both ways and one delegated turn | Canary receipt read back; health leaves `degraded` |
| G1 | Gemies | Voice mode opens `live` with `model: "default"` and answers every `delegation.requested` with the chat route's own turn (Gemies ADR 0041) | Gemies CI green, production canary passes |

## 9. Open points for the owner

1. **The OpenAI key.** Kaana holds one enabled `openai` key row (`610adcc8-…`), and no `openai` slug is
   served. A key row belongs to exactly one slug and no command copies ciphertext across slugs, so
   `openai-live` needs its own import from stdin: a new key in the same OpenAI project (preferred, so
   each can be revoked alone) or the same secret. The account must be on a paid tier (Build or higher).
2. **Paraphrase.** GPT-Live speaks commentary "paraphrasing it". A figure, an address or a name in the
   backend's answer can come out reworded. If that is not acceptable, the emulated path (§2) speaks the
   tool output closer to verbatim, at the Realtime models' price.
3. **Voices.** Confirm `marin` (female) and `cedar` (male) as the product voices for `gpt-live-1`.
4. **Grok.** Keep `xai-realtime` as an emulated second adapter (§6), or delete it.

## Consequences

- A call has one brain, the same one as text. Voice gets every tool, connector and gemie that text has,
  with no copy to keep in step.
- A spoken answer waits for a backend turn. The acknowledgement covers the gap, and the client streams
  its answer in chunks so the voice starts on the first sentence.
- Changing the voice provider is a catalogue default in Oxy, proven safe by `TestLiveContract`.
- The provider's raw delegation, transcript stitching and append limits never reach a client.
