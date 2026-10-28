# ADR 0006 — Image input: inline only, bounded per line, billed as input tokens

- Status: accepted (the owner's decision of 2026-10-28 to let Alia see a tab
  screenshot and read scanned PDF pages); serving is gated by the rollout
  order below.
- Date: 2026-10-28
- Contract: set 8.1.0 (`@kaana.ai/contracts` 8.1.0).
- Oxy's half (who may send images and on which terms): Oxy ADR 0051.

## Context

The contract has carried an `image` content part since it moved here (ADR 0002),
and both request adapters already translate it. Nothing bounded it: an image
was forwarded by URL or inline, of any size and format, in any number. Oxy's
old edge refused every non-text part (`unsupported_modality`), because no hold
sized from the request could bound what an image costs. The grant lane did not
refuse it, and granted it as text: on a line that cannot see, and with a hold
that ignored it.

Every provider behind Kaana's image-capable lines bills an image as INPUT
TOKENS at the line's input price. OpenRouter publishes no per-image price for
any of them except Google's, whose `pricing.image` equals `pricing.prompt` per
token (the same reading that makes `pricing.audio` Google's own audio-token
rate; `GET https://openrouter.ai/api/v1/models`, read 2026-10-28). What one image
costs in tokens is each publisher's documented resize and tokenization rule,
which differs by model, not by provider.

## Decision

1. **An image is inline.** `source.kind` is `inline`: base64 the caller sent.
   Kaana fetches no image URL and forwards none for a provider to fetch. A URL
   would make Kaana or a provider download arbitrary content on the caller's
   behalf (a server-side request on our account, outside the route's data
   policy) and its size could not be bounded before the grant.
2. **The rules are the contract's**, `checkImageInput`, applied identically by
   Kaana's public API and its signed lane before anything is held: PNG, JPEG,
   WebP or GIF; canonical base64 of at most 3,750,000 bytes (5 MB base64, the
   smallest documented per-image limit, Anthropic on Amazon Bedrock); on a
   `user` or `tool` message; at most 20 a request (above 20 Anthropic applies a
   stricter per-image dimension limit to every image). A violation is
   `invalid_request` naming the part; nothing in it repeats the image.
3. **A line that takes images declares what one costs.** The declared model's
   internal `imageInput.maxInputTokensPerImage` is the most input tokens the
   line's provider bills for one image the contract admits, at any `detail` the
   contract offers, taken from the publisher's own documentation and cited in
   the commit. A line without it is undeclared: no request carrying an image is
   authorized on it, whatever `inputModalities` says. `GET /v1/models` does not
   serve it, so no public client parses a field it does not know.
4. **Kaana tells Oxy how many.** The grant fact `inputImages` is the count,
   present exactly when there is one. The image bytes are not in
   `estimatedInputTokens`. Oxy admits only lines that declare `imageInput` and
   bounds the input at the text estimate plus `inputImages` times the line's
   bound. Usage is unchanged: the provider's own input-token count, image
   tokens included, settles at the line's `input_tokens` price. No new unit.
5. **What the contract offers, Kaana serves.** A `tool` message carrying an
   image (a screenshot a tool returned) is native on the Messages API. On chat
   completions, whose tool role takes text only, Kaana sends the tool's text as
   its result and the images in a `user` message right after the run of tool
   results, so the conversation the model reads is the same.
6. **An image is payload.** It is never persisted, logged, put in an error,
   a usage record or a telemetry event, exactly as a prompt is not.

## Consequences

- Rollout order: `@kaana.ai/contracts` 8.1.0 is published first (a package
  release, no serving change); Oxy adopts it and deploys, since a consumer on
  8.0.0 refuses a declared catalogue carrying `imageInput` and an authorization
  carrying `inputImages`; then Kaana deploys the rules and the declarations.
- A model gains image input by a reviewed catalogue commit with its bound and
  its primary source, never by discovery.
- A caller with an image at a URL downloads it and sends it inline.

## Alternatives considered

- **Bound by the context window.** Sound with no per-model evidence, but it
  quotes every image request at the line's whole context and prices most lines
  out of any per-request ceiling.
- **A per-image usage unit.** No provider behind these lines bills one; it
  would be a second price for the same tokens.
- **Forward URLs to the provider.** Moves the fetch, not the risk: the provider
  downloads on our account, and the request is unbounded until it does.
