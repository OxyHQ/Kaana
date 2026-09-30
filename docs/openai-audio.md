# OpenAI audio adapter

Kaana serves OpenAI's synchronous file transcription through the `openai-audio`
provider and its native `openai_audio` protocol. It supports
`POST /v1/audio/transcriptions` only: not Chat Completions audio output, not
speech synthesis, not Realtime or Live sessions.

## Why a second slug for one origin

A provider slug resolves to exactly one adapter, and a transcription is a
different request family from Chat Completions: a multipart upload answered by
one JSON document. Serving it under `openai` would mean branching the chat
adapter on the request family. Under its own slug the boundary is structural: a
deployment routes to one adapter, so a transcription model published under
`openai-audio` can only be executed by this adapter, and a chat model can never
reach it. The slug is bound to `https://api.openai.com/v1` and to this protocol;
no other origin or slug is accepted.

The credential is an ordinary platform key row under `openai-audio`, imported
and bound through the existing PostgreSQL/KMS credential path. When it is the
same OpenAI account as the `openai` key, the two pools still retire
independently: an account-wide `insufficient_quota` is learned by each pool
from its own refusal. Kaana does not infer that two slugs' keys share an
account, and never treats them as independent capacity on that account's
behalf.

## Request mapping

- `audio_transcriptions`, audio modality, one user message with one inline
  base64 audio part of at most 20 MiB (the contract's ceiling; OpenAI accepts
  25 MB). WAV, MP3, FLAC, Ogg, WebM and MP4/M4A containers are accepted; the
  upload's file name carries the matching extension because OpenAI infers the
  container from it.
- The multipart form carries exactly `model` (the signed upstream id), `file`
  and `response_format`: `json`, or `verbose_json` for `whisper-1`, whose
  `json` response carries no duration. No language, prompt, temperature,
  timestamp or chunking default is invented.
- The transcript is emitted as one `output_text` delta.

Streaming (`stream: true`, OpenAI's `transcript.text.delta` events), remote
audio URLs, speech, reasoning, tools, sampling and structured output are refused
before anything is sent.

## Usage

OpenAI reports one of two usage shapes, and each is copied exactly:

- `duration` (whisper-1, and per-minute-priced models such as
  `gpt-transcribe`): seconds of audio, reported as `audio_input_milliseconds`,
  rounded up.
- `tokens` (token-priced models such as `gpt-4o-transcribe`): `input_tokens`
  and `output_tokens`. OpenAI's input count is mostly audio tokens
  (`input_token_details` splits audio from text); the contract has no
  audio-token unit, so the total is reported as `input_tokens` without the
  split. That is the provider's own count, not an estimate. What is lost is the
  price distinction, so a rate card for these deployments must price
  `input_tokens` at the audio-input rate. Faithful audio-token metering needs a
  contract unit (#90).

A response carrying neither shape, or an inconsistent one, is refused rather
than settled on a guess. Units are recorded before the transcript is delivered,
so a disconnected customer does not erase audio OpenAI already billed.

## Publishing

`configs/model-attribution.json` attributes the reviewed transcription ids to
`openai-audio` only. The publisher classifies every id in OpenAI's own namespace
(`openai`, `openai-audio`) by its documented request family and drops, with a
warning, any attributed id whose family the slug's adapter cannot execute.
Realtime, Live and audio-output ids are not expressible by any family and are
dropped under every slug. `TestOpenAITranscriptionIsAttributedOnlyToTheTranscriptionAdapter`
fails if a checked-in OpenAI row would be dropped by that gate.

## Operator configuration and enablement

Add `openai-audio` to `KAANA_PROVIDERS` (and to `KAANA_DISCOVERY_PROVIDERS` with
a `KAANA_PROVIDER_OPENAI_AUDIO_DISCOVERY_KEY_ID` to publish its deployments).
Health never uploads audio and stays degraded until a signed transcription
canary with an explicitly funded credential has been validated; a catalogue
entry is not evidence that the account can transcribe.

## Validation and sources

The real HTTP fake tests parse the multipart upload exactly as OpenAI would, and
cover both usage shapes, whisper's verbose duration, downstream failure,
invalid and oversized responses, container and size bounds, refusal
classification by OpenAI's error type, credential redaction and cancellation.
They use synthetic credentials and assert no live account behaviour.

Wire behaviour reviewed against OpenAI documentation on 2026-09-30:

- [Create transcription](https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create)
- [gpt-transcribe](https://developers.openai.com/api/docs/models/gpt-transcribe),
  [gpt-4o-transcribe](https://developers.openai.com/api/docs/models/gpt-4o-transcribe),
  [gpt-4o-mini-transcribe](https://developers.openai.com/api/docs/models/gpt-4o-mini-transcribe),
  [whisper-1](https://developers.openai.com/api/docs/models/whisper-1)
- [Error codes](https://platform.openai.com/docs/guides/error-codes)
