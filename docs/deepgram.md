# Deepgram voice adapter

Kaana includes the `deepgram` provider using the native `deepgram_voice`
protocol. It supports synchronous REST voice requests, not chat or realtime
WebSocket sessions. This is serving support; it does not publish model inventory
or prove that a production account has entitlement or credits.

## Request mapping

- `audio_speech`, audio modality: text input, an explicit MP3 response format,
  and a voice identical to the signed route's Aura upstream model. Requests
  contain 1–2000 Unicode characters. Kaana posts JSON to `/v1/speak`, validates
  the MP3 response, and emits bounded audio chunks. The `dg-char-count` header
  supplies measured character usage even when body delivery fails.
- `audio_transcriptions`, audio modality: one user message with one inline
  base64 audio part, at most 20 MiB. WAV, MP3, FLAC and Ogg containers are
  accepted. Kaana posts the audio bytes to `/v1/listen` with the exact signed
  model and emits the single transcript. Response metadata supplies audio input
  milliseconds, rounded up; only single-channel results are supported.

Streaming, remote audio URLs, speed controls, reasoning, tools, chat controls
and alternative output formats are refused before sending a request. The
adapter adds no language, diarization, punctuation or model defaults. A caller
needing those controls requires a separate contract and wire review.

## Operator configuration and enablement

Add `deepgram` to the existing `KAANA_PROVIDERS` list. The reviewed default is
`https://api.deepgram.com/v1`; `KAANA_PROVIDER_DEEPGRAM_BASE_URL` may select
`https://api.eu.deepgram.com/v1`. Other endpoints and HTTP redirects are refused.
Credentials use `Authorization: Token` only at send time, through the existing
PostgreSQL/KMS credential store and exact deployment binding. Never place the
secret in environment variables, repository configuration or logs.

Account discovery is `not_available`: public models are not account entitlement.
Configured health remains degraded until operators validate an authorized voice
canary; health checks do not spend credits. Before production enablement, import
the credential through the existing credential administration path, review and
publish an immutable model deployment through Oxy, bind the exact credential,
and validate a signed request and usage receipt. An Aura voice name alone does
not establish immutable model identity. Do not invent or publish a model
reference from the examples in tests.

## Validation and sources

The real HTTP fake tests cover both request formats, measured units, failed
downstream delivery, invalid responses, cancellation, refusal classification,
credential redaction and unsupported controls. They use synthetic credentials
and do not assert live account conformance.

Wire behavior reviewed against Deepgram documentation on 2026-09-27:

- [Speech synthesis request](https://developers.deepgram.com/reference/text-to-speech/speak-request)
- [Prerecorded transcription request](https://developers.deepgram.com/reference/speech-to-text/listen-pre-recorded)
- [Speech encoding](https://developers.deepgram.com/docs/tts-encoding)
- [Errors](https://developers.deepgram.com/docs/errors)
