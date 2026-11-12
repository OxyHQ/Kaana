# Realtime findings

> **Historical record.** Moved out of [realtime.md](realtime.md), which keeps the wire, the adapters and
> how to operate them. This is what was found, refused and left unverified while the realtime
> adapters were built; verify against the code and a live provider before relying on any line.

## Findings, refusals and what is not verified

1. **Transcription sessions are served by `xai-stt` (ADR 0006); translation
   sessions, and transcription sessions on OpenAI, are refused** for `openai-realtime`
   (`unsupported_modality`, `kind`). OpenAI's translation is a separate endpoint
   (`/v1/realtime/translations`) and event protocol with no items, no
   responses and no usage event (billed per minute of audio); the contract's
   `output_audio.delta` needs a response and an item it would have to invent.
   OpenAI documents transcription sessions (`session.type: "transcription"`)
   but no WebSocket endpoint for them — only a beta-shaped REST route that
   mints a token. Both would be guesses from documentation.
2. **Input transcription in a conversation is served** since contract set
   4.1.0 (it was refused before): the ASR model's charge is its own unit,
   `input_transcription_milliseconds`, and the transcription model is declared
   per slug by the dialect ("Input transcription"). The signed-envelope lane
   still refuses it.
3. **`temperature` has no GA field**; contract set 5.0.0 removed it.
4. **Text and audio output together** is emulated on OpenAI: GA takes exactly
   one output modality per response, so the response is asked for audio and
   its transcript is relayed as its text too.
5. **Reasoning tokens** of the reasoning Realtime models are not split out of
   `output_tokens`: GA usage documents no reasoning breakdown. UNVERIFIED
   whether one exists on the wire.
6. **UNVERIFIED:** whether OpenAI refuses a bad key on the handshake (HTTP 401,
   as the adapter classifies) or in-band (an `error` event, also classified);
   the codes it uses in-band for quota and rate limits; what it sends when its
   own 60-minute session limit elapses (treated as whatever close follows).
7. **The catalogue surfaces realtime capability** from Kaana's own inventory
   since contract set 4.1.0: the adapter's reviewed statement, in each session
   deployment's `observed` block ("What the catalogue says about a realtime
   model").
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
10. **xAI retired `grok-voice-think-fast-2.0` (resolved 2026-10-16).** From the
    2026-10-14T16:34Z publication xAI answered the discovery session for it on
    another model, so the line left the snapshot — silently, because a
    "not served" answer was not logged — and every Alia voice call failed with
    no realtime model. xAI's models page and Voice API now name
    `grok-voice-3.0` (current), `grok-voice-2.0` (deprecated) and the alias
    `grok-voice-latest`, at the same $0.08/min. `grok-voice-3.0` is now the
    attributed line, with its own catalogue line, rate card and commercial
    entry; and a "not served" answer is logged as a warning naming the model
    and xAI's answer (`DiscoveredModel.NotServed`).

11. **UNVERIFIED for `xai-stt`:** whether xAI bills streaming by audio or by
    connection time (the meter's wall clock bounds both; the canary reads xAI's
    console), its idle timeout, its close codes, its in-band rate or quota
    signals (`error` carries only a message, so it is never classified as a
    credential or quota verdict), the shape of `words`, and whether
    `speech_final` text always extends the locked chunks (when it does not,
    `transcript.done` carries xAI's final text and no delta reconciles it).
