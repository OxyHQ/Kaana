# OpenAI audio adapter

Kaana serves OpenAI's audio APIs through the `openai-audio` provider and its
native `openai_audio` protocol:

- synchronous file transcription, `POST /v1/audio/transcriptions`
  (`audio_transcriptions`);
- audio chat, `POST /v1/chat/completions` answered aloud (`chat_completions`
  with the contract's `audioOutput`, contract set 3.2.0).

Not speech synthesis, and not Realtime or Live sessions.

The same spoken-chat wire also serves OpenRouter's `openai/gpt-audio` and
`openai/gpt-audio-mini` rows through the `openaicompat` adapter ("OpenRouter's
audio chat rows" below); OpenAI-direct stays the paid fallback behind them.

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
OpenAI's own namespace (`openai`, `openai-audio`, and OpenRouter's `openai/`
rows) by its documented request family and, for `chat_completions`, whether the
answer is spoken (`providerconfig.ClassifyModel`). It drops, with a warning, any
attributed id whose family the slug's adapter cannot execute: an audio chat id
under `openai`, a text chat id under `openai-audio`
(`providerconfig.ChatOutputs`). Realtime ids are session models: conversation
models are published only under `openai-realtime` (docs/realtime.md) and
dropped here; GPT-Live ids are dropped under every slug.
`TestOpenAIAudioIsAttributedOnlyToTheAudioAdapter` fails if any checked-in row
would be dropped by that gate.

`gpt-audio-1.5` is attributed by its only snapshot id. `gpt-audio`,
`gpt-audio-mini` and `gpt-4o-audio-preview` are not: OpenAI has retired them or
scheduled them for shutdown with `gpt-audio-1.5` as the replacement.

## Speaking is per deployment

Whether a chat is answered aloud is decided per slug AND model, once, in
`providerconfig`, and read by both commands:

- `ChatOutputs(slug, protocol)` is what an adapter's chat_completions path can
  produce: `openai-audio` speaks only, `openrouter` writes and speaks, every
  other chat adapter writes only.
- `SpeaksAloud(slug, protocol, upstreamModelID)` adds the model: the adapter
  must speak AND the model must be one OpenAI documents as answering aloud.
- The publisher attaches a spoken model only where its slug speaks.
- The executor asks the adapter (`provider.ChatOutputDeclarer`) before
  `Translate`, exactly as it asks `provider.Executes`: a spoken request to a
  deployment that cannot answer aloud is refused `unsupported_modality`
  (`audioOutput`), and a text chat to a speak-only deployment `invalid_request`,
  with nothing sent upstream (`TestTheExecutorDecidesSpeechPerDeployment`).
  Each adapter refuses the same thing in `Translate` as a second line.

The wire itself — request mapping, the stream and whole-answer readers, the
audio chunking and ceiling, and the audio-token partition — is one package,
`internal/provider/spokenchat`, shared by this adapter and OpenRouter's.

## OpenRouter's audio chat rows

OpenRouter serves OpenAI's audio chat models under its own namespace,
`openai/gpt-audio` and `openai/gpt-audio-mini`, both already attributed and
published under `openrouter`. They answer aloud through the `openaicompat`
adapter on the shared spoken wire; a text chat to the same deployment still
runs on the text path. What is OpenRouter's own:

- **Streaming only.** "Audio output requires streaming (`stream: true`)." The
  upstream is always streamed. A customer who asked for the whole answer (any
  format) receives the same normalized events, which the edge folds; a
  streamed customer request is `pcm` as the contract requires.
- **The provider policy** (`zdr`, `data_collection: deny`,
  `require_parameters: true`) rides on the body exactly as on a text request.
- **Usage is always included** in the last SSE chunk ("`stream_options:
  {include_usage: true}` ... deprecated and have no effect"); it is still sent,
  harmlessly, because the shared wire sends it. `prompt_tokens_details.audio_tokens`
  and `completion_tokens_details.audio_tokens` nest exactly as OpenAI's, so the
  partition above applies unchanged; OpenRouter's `cost` field is not read.
- In-stream `error` objects are classified by the `openaicompat` vocabulary.
- Voices and formats are sent as the caller named them; OpenRouter lists
  `alloy`, `echo`, `fable`, `onyx`, `nova`, `shimmer` and `wav`, `mp3`,
  `flac`, `opus`, `pcm16` as examples that "vary by model".

List prices from `GET https://openrouter.ai/api/v1/models` on 2026-09-30 (USD
per token; `audio` is audio input, `audio_output` audio output):

| | prompt | completion | audio | audio_output |
|---|---|---|---|---|
| `openai/gpt-audio` | 0.0000025 | 0.00001 | 0.000032 | 0.000064 |
| `openai/gpt-audio-mini` | 0.0000006 | 0.0000024 | 0.0000006 | 0.0000024 |

A rate card for these deployments prices `audio_input_tokens` at `audio` and
`audio_output_tokens` at `audio_output`, the text units at `prompt` and
`completion`.

Not verified from OpenRouter's documentation, and so what the first signed
canary must confirm: whether `require_parameters: true` counts `modalities` and
`audio` (neither is in either model's `supported_parameters`, and OpenRouter's
parameter enum has no audio entry — if it counts them, OpenRouter answers 404
for "no endpoints" and the route fails as `model_not_found`); whether a
non-`pcm16` format streams through OpenRouter (its own example streams `wav`;
OpenAI documents streaming as `pcm16` only); and whether a stream chunk carries
`id` and `expires_at`, which this reader ignores. Both OpenRouter models are
OpenAI's `gpt-audio` and `gpt-audio-mini`, which OpenAI has scheduled for
shutdown; OpenRouter's catalogue will withdraw them then and the publisher will
stop publishing them.

Sources: https://openrouter.ai/docs/guides/overview/multimodal/audio ,
https://openrouter.ai/docs/api/api-reference/chat/create-a-chat-completion ,
https://openrouter.ai/docs/cookbook/administration/usage-accounting ,
https://openrouter.ai/docs/guides/routing/provider-selection ,
https://openrouter.ai/api/v1/models ,
https://openrouter.ai/openai/gpt-audio , https://openrouter.ai/openai/gpt-audio-mini

## Operator configuration and enablement

Add `openai-audio` to `KAANA_PROVIDERS` (and to `KAANA_DISCOVERY_PROVIDERS` with
a `KAANA_PROVIDER_OPENAI_AUDIO_DISCOVERY_KEY_ID` to publish its deployments).
Health never uploads audio or asks for speech, and stays degraded until a signed
canary with an explicitly funded credential has been validated; a catalogue
entry is not evidence that the account can transcribe or speak.

OpenRouter's spoken rows need no new slug, key or discovery: `openrouter` is
already served and discovered, and `openai/gpt-audio` and
`openai/gpt-audio-mini` are already published. What enabling them needs is a
rate card for each of those deployments pricing `audio_input_tokens` and
`audio_output_tokens` (in 10⁻¹² USD per token: `32000000` / `64000000` for
`gpt-audio`, `600000` / `2400000` for `gpt-audio-mini`, beside `input_tokens`
and `output_tokens` at `2500000` / `10000000` and `600000` / `2400000`), and
Oxy signing `audioOutput` requests to them, which it may do once its catalogue
advertises those deployments as speaking.

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
