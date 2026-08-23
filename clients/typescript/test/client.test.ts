/**
 * The client, driven through an injected `fetch`. No network is opened here.
 *
 * The signature case is the one that matters most and the easiest to write
 * vacuously: it captures the body the client actually TRANSMITTED and verifies
 * the transmitted signature over it. A test verifying over the envelope it had
 * built instead would pass even if the client signed one serialisation and sent
 * another — exactly the bug worth catching, because in production it surfaces as
 * a blanket `authentication_failed` that names nothing. It is paired with a
 * negative control, since "the signature verified" is also what a verifier that
 * ignores the body reports.
 */

import {
  createHash,
  generateKeyPairSync,
  verify as cryptoVerify,
  type KeyObject,
} from 'node:crypto';
import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  KaanaClient,
  KAANA_HEALTH_PATH,
  KAANA_INFERENCE_PATH,
  KAANA_REQUEST_ID_HEADER,
} from '../src/client.js';
import { KaanaInferenceError, KaanaProtocolError, KaanaTransportError } from '../src/errors.js';
import { ed25519PrivateKeyFromSeed, ed25519PublicKeyFromRaw } from '../src/signing.js';
import { KAANA_FRAME_STREAM_EVENT, KAANA_FRAME_USAGE_REPORT } from '../src/stream.js';
import {
  chatEnvelope,
  deltaEvent,
  DONE_EVENT,
  ERROR_EVENT,
  GENERATION_ID,
  HEALTH_RESPONSE,
  REASONING_EVENT,
  REQUEST_ID,
  ROUTE_SWITCH_EVENT,
  sseWire,
  START_EVENT,
  TOOL_CALL_EVENTS,
  USAGE_EVENT,
  USAGE_REPORT,
} from './fixtures.js';

const BASE_URL = 'https://relay.oxy.so';
const TIMESTAMP = 1_755_990_000_123;

interface Captured {
  readonly url: string;
  readonly method: string;
  readonly headers: Record<string, string>;
  readonly body: Uint8Array;
}

interface Harness {
  readonly client: KaanaClient;
  readonly calls: Captured[];
  readonly publicKey: KeyObject;
}

function sseResponse(wire: string, chunkSize = 7): Response {
  const bytes = new TextEncoder().encode(wire);
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      // Deliberately not one chunk: a client that only works on a whole response
      // is a client that works in this file and nowhere else.
      for (let at = 0; at < bytes.length; at += chunkSize) {
        controller.enqueue(bytes.subarray(at, at + chunkSize));
      }
      controller.close();
    },
  });
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
}

function harness(respond: (call: Captured) => Response | Promise<Response>): Harness {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  const seed = privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32);
  const rawPublicKey = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  const calls: Captured[] = [];

  const client = new KaanaClient({
    baseUrl: BASE_URL,
    signingKey: {
      keyId: 'edge-2026-08',
      privateKey: ed25519PrivateKeyFromSeed(seed.toString('base64')),
    },
    now: () => TIMESTAMP,
    fetch: async (input, init) => {
      const headers: Record<string, string> = {};
      for (const [name, value] of Object.entries(init?.headers ?? {})) {
        headers[name] = String(value);
      }
      const body = init?.body;
      const call: Captured = {
        url: String(input),
        method: init?.method ?? 'GET',
        headers,
        body: body instanceof Uint8Array ? body : new Uint8Array(0),
      };
      calls.push(call);
      return await respond(call);
    },
  });

  return { client, calls, publicKey: ed25519PublicKeyFromRaw(rawPublicKey.toString('base64')) };
}

/** `edgeauth.Verify`, over the bytes that were actually transmitted. */
function verifyTransmitted(call: Captured, publicKey: KeyObject): boolean {
  const digest = createHash('sha256').update(call.body).digest('hex');
  const preimage = Buffer.from(
    [
      // The domain separator keeps the `relay` spelling because it is signed;
      // the headers do not, because they are sent. Both are spelled out here
      // rather than imported, so a rename on either has to be made twice.
      'oxy-relay-envelope:v1',
      call.headers['X-Oxy-Kaana-Key-Id'],
      call.headers['X-Oxy-Kaana-Timestamp'],
      digest,
    ].join('\n'),
    'utf8',
  );
  const signature = Buffer.from(
    (call.headers['X-Oxy-Kaana-Signature'] ?? '').replace(/^v1=/, ''),
    'base64',
  );
  return cryptoVerify(null, preimage, publicKey, signature);
}

const HAPPY_WIRE = sseWire([
  { name: KAANA_FRAME_STREAM_EVENT, payload: START_EVENT },
  { name: KAANA_FRAME_STREAM_EVENT, payload: ROUTE_SWITCH_EVENT },
  { name: KAANA_FRAME_STREAM_EVENT, payload: deltaEvent(2, 'Hola') },
  { name: KAANA_FRAME_STREAM_EVENT, payload: REASONING_EVENT },
  { name: KAANA_FRAME_STREAM_EVENT, payload: deltaEvent(4, '!') },
  { name: KAANA_FRAME_STREAM_EVENT, payload: TOOL_CALL_EVENTS[0] },
  { name: KAANA_FRAME_STREAM_EVENT, payload: TOOL_CALL_EVENTS[1] },
  { name: KAANA_FRAME_STREAM_EVENT, payload: USAGE_EVENT },
  { name: KAANA_FRAME_STREAM_EVENT, payload: DONE_EVENT },
  { name: KAANA_FRAME_USAGE_REPORT, payload: USAGE_REPORT },
]);

async function drain(client: KaanaClient): Promise<void> {
  for await (const _frame of client.stream(chatEnvelope())) {
    // The assertion is about the request, or about the rejection.
  }
}

describe('KaanaClient.stream', () => {
  it('signs the exact bytes it transmits', async () => {
    const { client, calls, publicKey } = harness(() => sseResponse(HAPPY_WIRE));

    await drain(client);

    assert.equal(calls.length, 1);
    const call = calls[0];
    assert.ok(call !== undefined);
    assert.equal(call.url, `${BASE_URL}${KAANA_INFERENCE_PATH}`);
    assert.equal(call.method, 'POST');
    assert.equal(verifyTransmitted(call, publicKey), true);
  });

  it('would not verify if the transmitted body were altered in flight', async () => {
    const { client, calls, publicKey } = harness(() => sseResponse(HAPPY_WIRE));

    await drain(client);

    // The negative control for the case above: without it, a verifier that
    // ignored the body would pass both.
    const call = calls[0];
    assert.ok(call !== undefined);
    const tampered: Captured = { ...call, body: new TextEncoder().encode('{"schemaVersion":1}') };
    assert.equal(verifyTransmitted(tampered, publicKey), false);
  });

  it('sends the envelope Kaana parses, not a re-encoded copy', async () => {
    const envelope = chatEnvelope();
    const { client, calls } = harness(() => sseResponse(HAPPY_WIRE));

    for await (const _frame of client.stream(envelope)) {
      // Drained.
    }

    assert.deepEqual(
      JSON.parse(new TextDecoder().decode(calls[0]?.body ?? new Uint8Array(0))),
      JSON.parse(JSON.stringify(envelope)),
    );
  });

  it('yields every frame, including the usage report', async () => {
    const { client } = harness(() => sseResponse(HAPPY_WIRE));

    const frames = [];
    for await (const frame of client.stream(chatEnvelope())) frames.push(frame);

    assert.equal(frames.length, 10);
    assert.deepEqual(frames.at(-1), { frame: KAANA_FRAME_USAGE_REPORT, report: USAGE_REPORT });
  });

  it('reads a contract error body from a non-200 as a refusal', async () => {
    const { client } = harness(
      () =>
        new Response(
          JSON.stringify({
            schemaVersion: 1,
            code: 'authentication_failed',
            message: 'the request is not a signed Oxy edge envelope',
            retryable: false,
            requestId: 'req_relay_local',
          }),
          { status: 401, headers: { 'Content-Type': 'application/json' } },
        ),
    );

    await assert.rejects(() => drain(client), (error: unknown) => {
      assert.ok(error instanceof KaanaInferenceError);
      assert.equal(error.failure.code, 'authentication_failed');
      assert.equal(error.retryable, false);
      return true;
    });
  });

  it('does not report a proxy error page as a refusal by the model', async () => {
    const { client } = harness(() => new Response('<html>502 Bad Gateway</html>', { status: 502 }));

    await assert.rejects(() => drain(client), KaanaProtocolError);
  });

  it('carries the id Kaana minted for a rejection, which is all there is to correlate on', async () => {
    const { client } = harness(
      () =>
        new Response('<html>502 Bad Gateway</html>', {
          status: 502,
          headers: { [KAANA_REQUEST_ID_HEADER]: 'req_relay_abcdefgh' },
        }),
    );

    await assert.rejects(() => drain(client), /req_relay_abcdefgh/);
  });

  it('reports a transport failure as one, without quoting the signed headers', async () => {
    const { client } = harness(() => {
      throw new Error('ECONNRESET');
    });

    await assert.rejects(() => drain(client), (error: unknown) => {
      assert.ok(error instanceof KaanaTransportError);
      assert.equal(error.message.includes('v1='), false);
      return true;
    });
  });
});

describe('KaanaClient.complete', () => {
  it('drains a stream into one answer, keeping the channels apart', async () => {
    const { client } = harness(() => sseResponse(HAPPY_WIRE));

    const completion = await client.complete(chatEnvelope({ stream: false }));

    assert.equal(completion.text, 'Hola!');
    assert.equal(completion.reasoning, 'thinking');
    assert.equal(completion.refusal, '');
    assert.equal(completion.finishReason, 'stop');
    assert.equal(completion.requestId, REQUEST_ID);
    assert.equal(completion.generationId, GENERATION_ID);
    assert.deepEqual(completion.start, START_EVENT);
    assert.deepEqual(completion.report, USAGE_REPORT);
  });

  it('surfaces the route switch rather than hiding a same-model failover', async () => {
    const { client } = harness(() => sseResponse(HAPPY_WIRE));

    const completion = await client.complete(chatEnvelope());

    assert.deepEqual(completion.routeSwitches, [ROUTE_SWITCH_EVENT]);
  });

  it('accumulates a streamed tool call into its whole argument text', async () => {
    const { client } = harness(() => sseResponse(HAPPY_WIRE));

    const completion = await client.complete(chatEnvelope());

    assert.deepEqual(completion.toolCalls, [
      { toolCallId: 'call_1', name: 'lookup', arguments: '{"q":"x"}', complete: true },
    ]);
  });

  it('reports metered units and their source, and never a price', async () => {
    const { client } = harness(() => sseResponse(HAPPY_WIRE));

    const completion = await client.complete(chatEnvelope());

    assert.deepEqual(completion.units, [
      { unit: 'input_tokens', quantity: 12 },
      { unit: 'output_tokens', quantity: 2 },
    ]);
    assert.equal(completion.usageSource, 'provider_reported');
  });

  it('throws the terminal error event, carrying the contract retryability', async () => {
    const wire = sseWire([
      { name: KAANA_FRAME_STREAM_EVENT, payload: START_EVENT },
      { name: KAANA_FRAME_STREAM_EVENT, payload: ERROR_EVENT },
    ]);
    const { client } = harness(() => sseResponse(wire));

    await assert.rejects(
      () => client.complete(chatEnvelope()),
      (error: unknown) => {
        assert.ok(error instanceof KaanaInferenceError);
        assert.equal(error.failure.code, 'provider_overloaded');
        assert.equal(error.retryable, true);
        assert.equal(error.failure.retryAfterMs, 1500);
        return true;
      },
    );
  });

  it('refuses a stream that ended without a terminal event', async () => {
    const wire = sseWire([
      { name: KAANA_FRAME_STREAM_EVENT, payload: START_EVENT },
      { name: KAANA_FRAME_STREAM_EVENT, payload: deltaEvent(1, 'Hol') },
    ]);
    const { client } = harness(() => sseResponse(wire));

    await assert.rejects(() => client.complete(chatEnvelope()), KaanaTransportError);
  });

  it('still answers when the usage report frame never arrived', async () => {
    const wire = sseWire([
      { name: KAANA_FRAME_STREAM_EVENT, payload: START_EVENT },
      { name: KAANA_FRAME_STREAM_EVENT, payload: deltaEvent(1, 'Hola') },
      { name: KAANA_FRAME_STREAM_EVENT, payload: DONE_EVENT },
    ]);
    const { client } = harness(() => sseResponse(wire));

    const completion = await client.complete(chatEnvelope());

    assert.equal(completion.text, 'Hola');
    assert.equal(completion.report, undefined);
  });
});

describe('KaanaClient.health', () => {
  it('signs an empty body on a GET and reads the projection', async () => {
    const { client, calls, publicKey } = harness(
      () =>
        new Response(JSON.stringify(HEALTH_RESPONSE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );

    const health = await client.health();

    const call = calls[0];
    assert.ok(call !== undefined);
    assert.equal(call.url, `${BASE_URL}${KAANA_HEALTH_PATH}`);
    assert.equal(call.method, 'GET');
    assert.equal(call.body.length, 0);
    assert.equal(verifyTransmitted(call, publicKey), true);
    assert.equal(health.contractVersion, '1.1.0');
    assert.equal(health.configuration.servesUnpinnedReferences, true);
    assert.equal(health.providers[0]?.credentials?.usable, 1);
    assert.equal(health.deployments[1]?.state, 'open');
  });

  it('tolerates a field the operator surface has grown', async () => {
    const { client } = harness(
      () => new Response(JSON.stringify({ ...HEALTH_RESPONSE, queueDepth: 3 }), { status: 200 }),
    );

    const health = await client.health();

    assert.equal(health.contractVersion, '1.1.0');
  });

  it('refuses a projection missing what a caller reads it for', async () => {
    const { configuration: _dropped, ...withoutConfiguration } = HEALTH_RESPONSE;
    const { client } = harness(
      () => new Response(JSON.stringify(withoutConfiguration), { status: 200 }),
    );

    await assert.rejects(() => client.health(), KaanaProtocolError);
  });
});

describe('the base url', () => {
  it('joins cleanly when it carries a trailing slash', async () => {
    const { privateKey } = generateKeyPairSync('ed25519');
    const seed = privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32);
    let seen = '';

    const client = new KaanaClient({
      baseUrl: `${BASE_URL}///`,
      signingKey: {
        keyId: 'edge-2026-08',
        privateKey: ed25519PrivateKeyFromSeed(seed.toString('base64')),
      },
      fetch: (input) => {
        seen = String(input);
        return Promise.resolve(sseResponse(HAPPY_WIRE));
      },
    });

    await client.complete(chatEnvelope());

    assert.equal(seen, `${BASE_URL}${KAANA_INFERENCE_PATH}`);
  });
});
