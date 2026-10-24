# @kaana.ai/sdk

The TypeScript client of Kaana's public API, `https://kaana.ai/v1`. It runs in
Node 20+, browsers and React Native, validates every response against
[`@kaana.ai/contracts`](https://www.npmjs.com/package/@kaana.ai/contracts) and
reports failures as typed errors.

Applications, API keys (`oxy_sk_…`), limits, budgets, usage and billing are
managed in [Oxy Console](https://console.oxy.so). Kaana has no console and
issues no keys of its own.

```sh
npm install @kaana.ai/sdk
```

## Credentials

The credential is the value of `Authorization: Bearer`: a string, or a
function called on every request. The SDK never caches it; whoever supplies
the function already caches and renews.

```ts
import { Kaana } from '@kaana.ai/sdk';

new Kaana({ credential: process.env.KAANA_API_KEY! });                 // oxy_sk_ from Console
new Kaana({ credential: async () => (await oxy.getAccessToken())! });  // an Oxy app session
```

Options: `baseURL` (only `https://kaana.ai/v1`, or a loopback origin in
development), `fetch`, `WebSocket` (for realtime where there is no global
one, e.g. `ws` in older Node), `timeoutMs` (default 120 000) and `maxRetries`
(default 2: only before the first byte, only for retryable codes, honouring
`retryAfterMs`).

Every method takes `RequestOptions`: `signal`, `idempotencyKey`
(`Idempotency-Key`, reused by every retry), `onBehalfOfUserId`
(`X-Oxy-User-Id`, honoured only with a service token) and `timeoutMs`.

## Oxy-owned services

Use the existing workload-backed `OxyServer` with the server entry:

```ts
import { kaanaForOxyService } from '@kaana.ai/sdk/server';

const kaana = kaanaForOxyService(oxyServer, {
  attribution: { accountId: originalAccountId }, // optional, fixed for this client
});
await kaana.responses.create({ model: 'auto', input: 'Hola' });
// The same client also provides realtime.connect(...).
kaana.dispose(); // releases this transport, not the shared OxyServer
```

This factory requires the Oxy core 7 transport. Oxy owns workload attestation,
protocol 2, token renewal, cancellation and origin validation; the app supplies
no API key, scope list or token callback. Kaana still owns inference retries,
SSE, idempotency and the realtime session lifecycle. A resumed socket reuses the
original credential and protocols because the server binds resume to that
credential; only a new session obtains a current token.

`attribution` is immutable inference attribution, not permission to read an
account's private data or to charge that account. Oxy's signed admission keeps
the payer and application identity. An explicit requester proof can be supplied
instead where the caller already has one; the two options are exclusive.
Per-call `onBehalfOfUserId` must match the fixed account. Production uses only
`https://kaana.ai/v1`; loopback remains available for development.

External credentials continue using `new Kaana({ credential })`. This SDK
addition does not change the Go service's outbound Oxy producer protocol; see
[the rollout record](../../docs/first-party-transport.md).

## Methods

```ts
const kaana = new Kaana({ credential: process.env.KAANA_API_KEY! });

await kaana.models.list();
await kaana.models.get('elevenlabs/eleven-multilingual@v2');
await kaana.powerLevels.list();

await kaana.responses.create({ model: 'auto', input: 'Hola' });
const stream = kaana.responses.stream({ model: 'high', input: 'Cuéntame un cuento' });
for await (const event of stream) if (event.type === 'delta') process.stdout.write(event.text);
const response = await stream.final; // the folded Response

await kaana.chat.completions.create({ model: 'instant', messages: [{ role: 'user', content: 'Hola' }] });
kaana.chat.completions.stream({ model: 'instant', messages: [{ role: 'user', content: 'Hola' }] });

await kaana.embeddings.create({ model: 'openai/text-embedding-3-small', input: ['hola'] });
await kaana.images.generate({ model: 'openai/gpt-image-1', prompt: 'A fox' });
// `model` is optional: absent, the application's decisions profile chooses, and
// `decision.model` names the revision that answered. Answers that do not match
// the questions (or the pinned model) are a KaanaProtocolError.
await kaana.decisions.create({ state: '…', questions: [/* … */] });
await kaana.generations.get('gen_…');
await kaana.generations.getByIdempotencyKey('my-key');
```

A stream is iterated once. Breaking out of the loop, or `stream.abort()`,
cancels the request upstream. Awaiting `final` alone drains the stream.

The API is also compatible with the OpenAI SDK for chat completions:
`new OpenAI({ baseURL: 'https://kaana.ai/v1', apiKey: 'oxy_sk_…' })`.

## Audio

```ts
const speech = await kaana.audio.speech({
  model: 'elevenlabs/eleven-multilingual',
  input: 'Hola',
  voice: 'rachel',
});
speech.audio; // Uint8Array, speech.mediaType, speech.model (the served revision), speech.usage

const live = kaana.audio.speechStream({ model: 'elevenlabs/eleven-multilingual', input: 'Hola', voice: 'rachel' });
for await (const chunk of live) play(chunk); // mp3 or pcm, in order, as produced
const { model, usage } = await live.done;
live.toReadableStream(); // the same audio as a ReadableStream

await kaana.audio.transcriptions.create({
  model: 'openai/whisper-1',
  file: bytesOrBlob,        // a Blob is sent as multipart; a Uint8Array as JSON with base64
  mediaType: 'audio/wav',
  language: 'es-ES',
});
```

Voices, languages, formats, the `speed` range and the character limit of each
speech model are in `models.get(...).audio`; the server checks them before
anything is reserved.

## Realtime

```ts
const session = await kaana.realtime.connect({
  model: 'openai/gpt-realtime',
  type: 'session.open',
  kind: 'conversation',
  config: {
    outputModalities: ['audio'],
    voice: 'marin',
    inputAudioFormat: 'pcm16_24khz',
    outputAudioFormat: 'pcm16_24khz',
    turnDetection: { type: 'server_vad', createResponse: true, interruptResponse: true },
    tools: [{ type: 'function', name: 'lookup', parameters: { type: 'object' } }],
  },
});

session.appendAudio(pcm16);            // split into frames of at most 49 152 bytes
session.send({ type: 'response.create' }); // schemaVersion, requestId and commandId are filled in
for await (const event of session) {
  if (event.type === 'tool_call' && event.complete) {
    session.send({
      type: 'conversation.item.create',
      item: { type: 'function_call_output', callId: event.toolCallId, output: '{}' },
    });
  }
}
const usage = await session.close(); // settles exactly once
```

The credential travels as a WebSocket subprotocol (`kaana.bearer.…`), so
realtime works where a WebSocket cannot set headers (browsers, React Native).
If the connection drops, iteration throws a `KaanaConnectionError`; call
`session.resume()` within the session's resume window and iterate again:
what was missed is replayed once, in order.

## Errors

| Class | Codes |
|---|---|
| `KaanaAuthenticationError` | `authentication_failed` (keys are managed in [Oxy Console](https://console.oxy.so)) |
| `KaanaPermissionError` | `permission_denied`, `insufficient_scope`, `commercial_permission_denied`, `policy_violation` |
| `KaanaInvalidRequestError` | `invalid_request`, `model_not_found`, `unsupported_modality`, `context_length_exceeded`, `request_too_large`, `output_limit_exceeded`, `idempotency_conflict`, `byok_credential_invalid` |
| `KaanaBillingError` | `insufficient_balance`, `spending_limit_exceeded`, `quota_exceeded` |
| `KaanaRateLimitError` | `rate_limited` |
| `KaanaUnavailableError` | `no_route_available`, `service_unavailable`, `provider_error`, `provider_timeout`, `provider_overloaded` |
| `KaanaContentFilteredError` | `upstream_content_filtered` |
| `KaanaCancelledError` | `cancelled`, including a request you aborted |
| `KaanaInternalError` | `internal_error` |

Each `KaanaError` has `code`, `status` (`null` when it arrived as a terminal
stream event), `requestId`, `retryable`, `retryAfterMs`, `param` and
`afterOutput` (output reached you before the failure). `KaanaProtocolError` is a
response outside the contract and is never retried; `KaanaConnectionError` is
the network or a timeout (`timedOut`) before a response.

## Testing

`@kaana.ai/sdk/testing` runs the real client against `https://kaana.ai/v1` in
memory, so code that uses Kaana is tested without reaching it:

```ts
import { createFakeKaana } from '@kaana.ai/sdk/testing';

const { kaana, server } = createFakeKaana({
  transcribe: () => ({ text: 'hola', durationMs: 1200 }),
});
const session = await kaana.realtime.connect(open); // your session.open
server.sessions[0].emit({ type: 'input_audio.cleared' });
```

The fake decides with the same schemas and `check*` functions of
`@kaana.ai/contracts` Kaana applies before a grant, against `voiceCatalogue`
— what Kaana serves for its voice models, held to Kaana's own render — so a
request it accepts is one Kaana accepts, and a refusal arrives as the same
error class with the same `code` and `param`. A session records the commands
it applied (`commands`) and is driven by the test (`emit`, `drop`, `end`).

## React Native

Non-streaming methods work with React Native's built-in `fetch`. The streaming
methods (`responses.stream`, `chat.completions.stream`, `audio.speechStream`)
need a `fetch` whose response body is a `ReadableStream`; install a streaming
fetch polyfill and pass it as `fetch`. Without one they fail with a clear
`KaanaConnectionError` instead of buffering silently. Transcription of bytes
uses JSON with base64, so no `Blob` support is required. React Native's
`WebSocket` carries realtime sessions as is.

## License

Breathe License 1.0 — see `LICENSE` and `NOTICE`.
