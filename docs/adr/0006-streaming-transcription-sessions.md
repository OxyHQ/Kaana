# ADR 0006 — Streaming transcription sessions

- Status: accepted (2026-10-28). Deployment remains gated by the rollout in §6, the canary included.
- Authority: the owner's decision of 2026-10-28: word-by-word dictation through xAI's streaming speech-to-text at $0.20 an hour.
- Date: 2026-10-28
- Consumers: Alia's dictation (Alia ADR 0024, proposed), and through it Astro's command bar and Chat composer.

## Context

Dictation today is batch: `POST /v1/audio/transcriptions` takes one clip and answers its text
(`internal/publicapi/transcription.go`). It has run in production since 2026-10-16: Oxy has
admitted `alia-kaana` `audio_transcriptions` grants since 03:22 UTC that day, on
`x-ai/grok-voice-transcribe-2.0` and Groq's Whisper lines. A batch clip only answers once the
person stops. Astro works around that by cutting the recording at pauses and sending each phrase
as its own request. The text then arrives phrase by phrase, and each phrase is one grant against
`alia-kaana`'s 5 000 requests a UTC day, which all of Alia's inference shares.

The contract already defines a `transcription` session kind (`realtimeSessionKindSchema`). `realtimeConfigIssues` (`packages/contracts/src/public/realtime.ts:653`)
already holds what such a session must and must not configure. Oxy's grant API plans it too:
`kaanaGrant.service.ts:474` sizes a `realtime_session` grant of kind `transcription` as
audio in, text out. What is missing is a provider that opens one. `openai-realtime` refuses the
kind because OpenAI documents no WebSocket endpoint for it, and xAI's Voice Agent API holds
conversations only (`internal/providerconfig/providerconfig.go:193`).

xAI now documents a streaming speech-to-text WebSocket on the same account and key family
production already serves (https://docs.x.ai/developers/model-capabilities/audio/speech-to-text,
read 2026-10-28):

- `wss://api.x.ai/v1/stt`, `Authorization: Bearer`. Configuration is query parameters only:
  `model` (only `grok-voice-transcribe-2.0`), `encoding` (`pcm`, `mulaw`, `alaw`, `opus`),
  `sample_rate` (8000 … 48000), `interim_results`, `endpointing` (0–5000 ms), `language`.
- The client sends raw binary audio frames, `{"type":"finalize"}` (commit the current utterance)
  and `{"type":"audio.done"}` (end of audio).
- The server sends `transcript.created` (ready), then `transcript.partial` with `text`,
  `is_final` and `speech_final`. `is_final:false` is an interim hypothesis that may change,
  about every 500 ms. `is_final:true, speech_final:false` locks about 3 s of speech.
  `speech_final:true` is the complete utterance. After `audio.done` the server sends
  `transcript.done` with `text` and a `duration` that is always present, then closes. An `error`
  event carries a `message`.
- List price (https://docs.x.ai/developers/models): "Speech to Text … $0.20 / hr (Streaming)".

These are **not documented**: whether streaming bills audio duration or connection time, idle
timeouts, close codes, in-band rate or quota codes, and the shape of `words`.

## Decision

### 1. A new session slug, `xai-stt`, with its own adapter

`internal/provider/xaistt` is a `provider.RealtimeAdapter` that declares only `transcription`. Its
slug is `xai-stt`, with protocol `xai_stt`, root `wss://api.x.ai/v1/stt`, locked like the other
realtime slugs. The wire is not the OpenAI Realtime shape, so it is not a dialect of
`openairealtime`, and a slug stays exactly one adapter. Its key row is a platform credential under
`xai-stt`. It can be the same xAI team as `xai-realtime`, preferably as a second key so that
either key can be revoked alone. Each pool retires on its own refusals, as `docs/realtime.md`
describes for `xai-realtime`.

The publisher attributes `grok-voice-transcribe-2.0` under `xai-stt` from the same authenticated
voice read that already proves the key reaches xAI's voice API
(`configs/model-attribution.json`, "XAI SPEECH-TO-TEXT"). It opens no discovery session. The
line's realtime statement is `{transports: [websocket], sessionKinds: [transcription],
inputAudioTranscription: false, audioFormats: {input: [pcm16_24khz, g711_ulaw, g711_alaw],
output: []}}`. Its languages are `providerconfig.xAITranscription`'s table.

### 2. Translation of the contract

| Contract | xAI streaming STT |
|---|---|
| `session.open {kind: transcription}` | dial with `model`, `encoding`/`sample_rate` from `inputAudioFormat` (`pcm16_24khz` → `pcm`/24000, `g711_ulaw` → `mulaw`/8000, `g711_alaw` → `alaw`/8000), `language` (the provider's spelling via `LanguageFor`, none to detect), `interim_results` (below); `session.created` only after `transcript.created` |
| `turnDetection: none` | `endpointing=5000`; utterances end on `input_audio.commit` |
| `turnDetection: server_vad` (`createResponse` and `interruptResponse` false, as the contract requires) | `endpointing = silenceDurationMs` (Kaana's default 500 when unnamed), clamped to xAI's 0–5000 |
| `turnDetection: semantic_vad` | refused at open (`Unservable`, `config.turnDetection`): xAI's `smart_turn` is a different control, and approximating it would be a guess |
| `inputAudioTranscription.prompt` | not sent: xAI's stream takes no prompt (its `keyterm` is a term list, not context), the same rule as batch |
| `input_audio.append` | the decoded bytes as one binary frame, never re-paced or re-chunked |
| `input_audio.commit` | `{"type":"finalize"}`; `input_audio.committed` when the utterance's `speech_final` arrives |
| `input_audio.clear`, `session.update`, `conversation.*`, `response.*` | `ErrUnsupported`, a non-fatal refusal naming the field; nothing is written upstream |
| `session.close` | `{"type":"audio.done"}`, then wait at most 5 s for `transcript.done` before closing |
| `transcript.partial is_final:true` | `transcript.delta {source: input_audio}` carrying only the newly locked text: deltas only append |
| `transcript.partial speech_final:true` | `transcript.done` for that item: exactly one per item, and the item ends there |
| `transcript.partial is_final:false` | `transcript.interim` (§3) when asked for; otherwise nothing |
| `error` | `error {fatal: true}`, classified from the message by the adapter's own table, never from a status |

Item ids are Kaana's (`item_<n>` per utterance), because xAI names none. A `transcript.done`
carries only that utterance's words, as `@kaana.ai/contracts` 6.1.0 requires.

### 3. Interim hypotheses: an additive contract change

Contract set 6.0.0 makes `transcript.delta` append-only. xAI's interim text revises itself, so it
can never be a delta: emitting an interim as a delta would put words in the record that the
final transcript may contradict. Word-by-word display needs its own event:

- `realtimeInputTranscriptionSchema.interimResults?: boolean`, refused outside a `transcription`
  session.
- A new server event, `transcript.interim {source: 'input_audio', itemId, contentIndex, text}`.
  It replaces the item's previous interim, and only a session that asked for it receives it. It
  is display state: never settled, never stored, and superseded by that item's next delta or its
  `transcript.done`.

This is an additive minor release of `@kaana.ai/contracts`: the Zod schema first, then the
descriptor, then the Go types, regenerated and never hand-edited. The signed-envelope lane keeps
refusing `interimResults` until Oxy's edge accepts the event in the same rollout
(`docs/realtime.md`, "The wire in this document is fixed").

### 4. Metering and limits

The session is metered `oxy_measured` through `provider.RealtimeMeter` as
`audio_input_milliseconds`: the decoded audio Kaana forwarded, computed from the byte count at the
open's fixed encoding. That figure is exact and bounded by the signed `maxInputAudioBytes`. xAI's
`transcript.done.duration` is compared with it on every session. The larger of the two is
settled, and a gap of more than 1 % is logged, without content, as a billing-basis finding.
Because xAI does not document whether streaming bills audio or connection time, the canary (§6)
reads xAI's own usage for the canary sessions before any consumer is granted the kind. If xAI
bills connection time, the meter becomes `session_milliseconds`, which is the rule
`xai-realtime`'s `server_vad` already follows. It never changes billing basis once open.

The rate card prices `audio_input_milliseconds` at $0.20 an hour: 55 555 × 10⁻¹² USD per
millisecond, rounded down. The card is keyed by the deployment id that the production inventory
reads back after the publisher first observes the line, exactly as `rc_voice_2026_10_16` was.
The line is unsold (no `configs/deployment-commercial.json` entry) until a reviewed commit adds it
with that id.

The signed limits apply unchanged (`maxDurationMs`, `idleTimeoutMs`, `maxInputAudioBytes`). A
session settles exactly once.

### 5. Routing, retries and failover

These are unchanged. Routes are tried in signed order until one emits `transcript.created`, with
credential verdicts from the open through `provider.WalkAttempts`. After that nothing is retried
or failed over: the audio has gone upstream and cannot be resent. Resume is the existing
cookie-free owner relay (ADR 0005); a replay returns the buffered `transcript.*` events.

### 6. Rollout, in order

1. Contract minor release (`interimResults`, `transcript.interim`) with the descriptor and Go types.
2. The `xai-stt` adapter, its real-wire fake (`xaistttest`) and conformance, the providerconfig
   statement and the publisher attribution. Merged inert, because no `KAANA_PROVIDERS` names the slug.
3. Owner: import the `xai-stt` key row from stdin (`kaana-platform-credential-import --provider
   xai-stt …`) and add its id to `.github/credential-admin-operations.json`. Then oxy-infra's
   switch (`var.kaana_xai_stt_enabled`) adds the slug to `KAANA_PROVIDERS` and
   `KAANA_DISCOVERY_PROVIDERS`. In that order: the process refuses to start on a slug with no
   enabled credential.
4. Read back the deployment id from the production inventory. Then, in one reviewed commit, the
   rate card and the commercial entry.
5. A signed canary session: 60 s of speech, then the meter compared with xAI's console usage (§4).
   Health stays `degraded` until this has been validated.
6. Oxy: `alia-kaana`'s `realtime.sessionKinds` gains `transcription` (Oxy PR, owner approval of
   the terms).

## Consequences

- One dictation is one grant instead of one per phrase. Words appear about every 500 ms instead
  of at each pause.
- Streaming lists at twice batch's price ($0.20/hr against $0.10/hr). A 5-minute dictation costs
  at most about USD 0.017.
- A second xAI slug is a second key pool to keep funded and rotated.
- What remains unverified (billing basis, idle timeout, close codes, in-band rate codes) is
  recorded under "Findings" in `docs/realtime.md` when the adapter lands, and the canary resolves
  the billing basis before any grant.

## Alternatives considered

- **Emit interim text as `transcript.delta`.** Rejected: deltas append, and xAI's interim text
  revises itself.
- **Serve the kind from `xai-realtime`'s adapter.** Rejected: a different wire inside the
  `openairealtime` dialect breaks "one adapter per slug" and the dialect rule.
- **OpenAI transcription sessions.** Not served: OpenAI documents no WebSocket endpoint for them
  (`docs/realtime.md`, Findings 1), and `openai-realtime` is not in production.
- **ElevenLabs Scribe realtime.** It is a documented candidate on a slug production already runs.
  It would be a second adapter in a later ADR; one provider is enough to ship.
- **Keep phrase-chunked batch.** It stays as the clients' fallback, but it is not the primary
  path, because of its per-phrase grants and its latency.
