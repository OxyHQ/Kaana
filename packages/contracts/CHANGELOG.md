# Changelog

## 8.2.0

Image input (Kaana ADR 0007). Additive.

- `checkImageInput(messages)` returns a conversation's image count and every
  issue against the image rules: inline base64 only (no image URL is fetched or
  forwarded), `INPUT_IMAGE_MEDIA_TYPES` (PNG, JPEG, WebP, GIF), at most
  `MAX_INPUT_IMAGE_BYTES` (3,750,000) decoded bytes each, on `user` or `tool`
  messages, and at most `MAX_INPUT_IMAGES` (20) a request. An issue names where
  and why, never the image. `base64DecodedLength` counts without decoding.
- `/internal`: `declaredModelSchema.imageInput` (`declaredImageInputSchema`,
  `{ maxInputTokensPerImage }`), the most input tokens one image costs on the
  line; absent, no request carrying an image is authorized on it. The grant
  fact `inputImages`, present exactly when a completion carries images.

A consumer on 8.1.0 or earlier refuses a declared catalogue or an
authorization carrying the new members: Oxy adopts 8.2.0 before Kaana declares
`imageInput` or sends `inputImages`.

## 8.1.0

Streaming transcription sessions (Kaana ADR 0006). A `transcription` session
may ask for `inputAudioTranscription.interimResults: true` and then receives
`transcript.interim { source: 'input_audio', itemId, contentIndex, text }`:
the provisional text of the current item, each one replacing the last, never
settled and never part of the transcript. `transcript.delta` still only
appends, and each item still ends in exactly one `transcript.done`. A
`conversation` or `translation` asking for interim text is refused at open.
Additive: everything 8.0.x parses still parses.

## 8.0.1

Require Zod 4.6.7 or newer in major 4 to include the oversized-array validation
fix (GHSA-mrhx-c7xc-m3w5). String bounds retain the existing UTF-16 code-unit
contract through explicit per-schema checks; the Go descriptor, wire shapes
and public limits remain unchanged.

## 8.0.0

Authenticated and grant principals are discriminated by authority. First-party
principals carry no scopes; external principals retain explicit scope arrays.
Consumers must narrow the union before reading `scopes` or `inferenceScopes`.
Historical signed records without an authority field remain external.

The Kaana reader accepts existing external grants and the new first-party shape.
Deploy that reader before enabling Oxy's first-party emitter. Signature,
audience, billing, route and account boundaries remain enforced.
