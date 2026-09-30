# OpenAI audio adapter

Kaana serves OpenAI's audio APIs through the `openai-audio` provider and its
native `openai_audio` protocol:

- synchronous file transcription, `POST /v1/audio/transcriptions`
  (`audio_transcriptions`);
- audio chat, `POST /v1/chat/completions` answered aloud (`chat_completions`
  with the contract's `audioOutput`, contract set 3.2.0).

Not speech synthesis, and not Realtime or Live sessions.

## Why a second slug for one origin

A provider slug resolves to exactly one adapter. A transcription is a multipart
upload answered by one JSON document, and a spoken answer is audio plus a
transcript; the text chat adapter carries neither. Serving them under `openai`
would mean branching the text adapter on the request family and the output.
Under its own slug the boundary is structural: a deployment routes to one
adapter, so an audio model published under `openai-audio` can only be executed
by this adapter, and a text chat model can never reach it — this adapter
executes `chat_completions` ONLY to answer aloud, and refuses a chat request
without `audioOutput`. The slug is bound to `https://api.openai.com/v1` and to
this protocol; no other origin or slug is accepted.

The credential is an ordinary platform key row under `openai-audio`, imported
and bound through the existing PostgreSQL/KMS credential path. When it is the
same OpenAI account as the `openai` key, the two pools still retire
independently: an account-wide `insufficient_quota` is learned by each pool
from its own refusal. Kaana does not infer that two slugs' keys share an
account, and never treats them as independent capacity on that account's
behalf.

## Transcription

### Request mapping

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
audio URLs, `audioOutput`, speech, reasoning, tools, sampling and structured
output are refused before anything is sent.

### Usage

OpenAI reports one of two usage shapes, and each is copied exactly:

- `duration` (whisper-1, and per-minute-priced models such as
  `gpt-transcribe`): seconds of audio, reported as `audio_input_milliseconds`,
  rounded up.
- `tokens` (token-priced models such as `gpt-4o-transcribe`): `input_tokens`
  and `output_tokens`. OpenAI's input count is mostly audio tokens
  (`input_token_details` splits audio from text), and the total is reported as
  `input_tokens` without the split. That is the provider's own count, not an
  estimate. What is lost is the price distinction, so a rate card for these
  deployments must price `input_tokens` at the audio-input rate. Contract set
  3.2.0 added `audio_input_tokens`; moving the transcription split onto it is a
  separate change from audio chat, because it changes what existing
  transcription deployments are billed under.

A response carrying neither shape, or an inconsistent one, is refused rather
than settled on a guess. Units are recorded before the transcript is delivered,
so a disconnected customer does not erase audio OpenAI already billed.

## Audio chat

### Request mapping

A `chat_completions` request with audio modality, a messages input and
`audioOutput: {voice, format}` becomes one Chat Completions call with:

- `model` (the signed upstream id), `messages`, `modalities: ["text", "audio"]`
  and `audio: {voice, format}`. The voice is sent exactly as the caller named
  it; OpenAI validates it, and an unknown voice comes back as a non-retryable
  `invalid_request`. The format maps `wav`, `mp3`, `flac` and `opus` to
  themselves and `pcm` to OpenAI's `pcm16`.
- `stream` and, when streaming, `stream_options: {include_usage: true}`. The
  contract only lets `pcm` stream, and the adapter refuses anything else
  streamed as well.
- Only what the caller set: `max_completion_tokens` from `maxOutputTokens`,
  `temperature`, `top_p`, `frequency_penalty`, `presence_penalty`, `seed` and
  `stop`. No voice, format, sampling value or token ceiling is invented.
- Messages: `system`, `developer`, `user` and `assistant` turns. A single text
  part is sent as a string. A user turn may carry inline base64 audio as an
  `input_audio` part (`wav` or `mp3`, the formats OpenAI documents, at most
  20 MiB each).

Refused in `Translate`, before anything is spent: a chat request without
`audioOutput` (a text chat belongs to a text deployment), text modality, a
streamed format other than `pcm`, reasoning effort (these models are not
reasoning models), `top_k`, structured output, speech parameters, and — not
implemented yet, rather than stubbed — tools, tool turns and function calling
(OpenAI documents function calling for `gpt-audio-1.5`). Images, files, remote
audio URLs, audio in a non-user turn and input audio other than wav/mp3 are
refused with the part named.

### Output

- Audio is emitted as `audio` events, independently base64-encoded, at most
  49,152 bytes each. The media type follows the requested format:
  `wav` → `audio/wav`, `mp3` → `audio/mpeg`, `flac` → `audio/flac`,
  `opus` → `audio/ogg`, `pcm` → `audio/pcm`. `pcm` is raw 16-bit little-endian
  mono samples with no header.
- Streamed: each `delta.audio.data` is decoded and split into bounded chunks
  as it arrives; `delta.audio.transcript` is emitted as a `delta` on
  `output_audio_transcript`. Non-streamed: `message.audio.data` is the whole
  file, emitted in bounded chunks after its transcript.
- The transcript is NEVER emitted as `output_text`. Text the model writes rather
  than speaks (`content`) is `output_text`, and a refusal is `refusal`.
- One answer carries at most the contract's `MAX_INFERENCE_AUDIO_BYTES`
  (20 MiB), the most the edge can fold into one response; more is a provider
  error.

### Usage

OpenAI's `prompt_tokens` includes its cached and audio tokens and
`completion_tokens` its reasoning and audio tokens. The contract's units are
siblings that partition the request, so each is subtracted out of its parent:

```text
cached_audio_input_tokens = prompt_tokens_details.cached_tokens_details.audio_tokens
audio_input_tokens        = prompt_tokens_details.audio_tokens - cached_audio_input_tokens
cached_input_tokens       = prompt_tokens_details.cached_tokens - cached_audio_input_tokens
input_tokens              = prompt_tokens - cached_tokens - audio_input_tokens
audio_output_tokens       = completion_tokens_details.audio_tokens
output_tokens             = completion_tokens - reasoning_tokens - audio_output_tokens
```

plus `requests: 1`. Chat Completions documents no `cached_tokens_details`; when
it is absent no cached token is attributed to audio. A report whose partition
would go negative is internally inconsistent and is refused rather than clamped,
because clamping would bill more units than OpenAI counted. Audio is reported
as tokens only, never also as milliseconds.

When OpenAI reports no usage, the executor's estimate counts the prompt text
and the transcript as text tokens and never invents an audio-token or
audio-duration unit (`internal/kaana/usage_estimate.go`); it is labelled
`estimated` for settlement to reconcile.

A failure after the response started (an `error` frame, a cut connection)
returns the units measured so far with the classified failure.

## Errors

A refusal is classified by status and OpenAI's own error type and code, never
by message prose: `insufficient_quota` (type or code) and a 402 are the
platform account's billing refusal, any other 429 a rate limit, 401/403 a
refused platform credential, 404 an unknown model, 503 overload. An `error`
frame inside a stream has no status and is classified by its type and code.
The message reaches the customer after this key is removed by exact match and
the contract's redaction runs.

## Publishing

`configs/model-attribution.json` attributes the reviewed transcription ids and
`gpt-audio-1.5` to `openai-audio` only. The publisher classifies every id in
OpenAI's own namespace (`openai`, `openai-audio`) by its documented request
family and, for `chat_completions`, whether the answer is spoken. It drops, with
a warning, any attributed id whose family the slug's adapter cannot execute: an
audio chat id under `openai`, a text chat id under `openai-audio`
(`providerconfig.SpokenChatCompletions`). Realtime and Live ids are not
expressible by any family and are dropped under every slug.
`TestOpenAIAudioIsAttributedOnlyToTheAudioAdapter` fails if a checked-in OpenAI
row would be dropped by that gate.

`gpt-audio-1.5` is attributed by its only snapshot id. `gpt-audio`,
`gpt-audio-mini` and `gpt-4o-audio-preview` are not: OpenAI has retired them or
scheduled them for shutdown with `gpt-audio-1.5` as the replacement.

OpenRouter lists `openai/gpt-audio`, and a gateway row is published under the
text adapter, which the family gate does not classify. The text adapter
therefore refuses `audioOutput` itself, first thing in `Translate`
(`TestSpokenOutputIsNeverExecutedByTheTextAdapter`), so spoken output is only
ever executed here.

## Operator configuration and enablement

Add `openai-audio` to `KAANA_PROVIDERS` (and to `KAANA_DISCOVERY_PROVIDERS` with
a `KAANA_PROVIDER_OPENAI_AUDIO_DISCOVERY_KEY_ID` to publish its deployments).
Health never uploads audio or asks for speech, and stays degraded until a signed
canary with an explicitly funded credential has been validated; a catalogue
entry is not evidence that the account can transcribe or speak.

## Validation and sources

The real HTTP fake tests parse the multipart upload and the Chat Completions
body exactly as OpenAI would. They cover both transcription usage shapes,
whisper's verbose duration, streamed and whole spoken answers in every format,
the audio-token partition with cached audio, chunk bounds and the output
ceiling, downstream failure, a stream cut after usage, an error frame after a
200, invalid and oversized responses, refusal classification by OpenAI's error
type, credential redaction and cancellation with an uninterrupted control. An
executor test runs a spoken answer end to end and prices each audio unit at its
own rate. They use synthetic credentials and assert no live account behaviour.

Wire behaviour reviewed against OpenAI documentation on 2026-09-30:

- [Create transcription](https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create)
- [gpt-transcribe](https://developers.openai.com/api/docs/models/gpt-transcribe),
  [gpt-4o-transcribe](https://developers.openai.com/api/docs/models/gpt-4o-transcribe),
  [gpt-4o-mini-transcribe](https://developers.openai.com/api/docs/models/gpt-4o-mini-transcribe),
  [whisper-1](https://developers.openai.com/api/docs/models/whisper-1)
- [Audio in Chat Completions](https://developers.openai.com/api/docs/guides/audio-chat-completions),
  [Create chat completion](https://developers.openai.com/api/reference/resources/chat),
  [gpt-audio-1.5](https://developers.openai.com/api/docs/models/gpt-audio-1.5),
  [Deprecations](https://developers.openai.com/api/docs/deprecations)
- [Error codes](https://developers.openai.com/api/docs/guides/error-codes)

Not verified from the current documentation, and so the first thing a signed
canary must confirm: the current Chat Completions reference no longer documents
the streamed `delta.audio` object (`id`, `data`, `transcript`, `expires_at`) this
adapter reads, nor states that only `pcm16` streams or the `pcm16` sample layout
for chat (24 kHz mono is documented for speech and Realtime), nor whether
`stream_options.include_usage` reports audio tokens on the final chunk.
