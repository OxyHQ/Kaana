import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import type { Server } from 'bun';
import { OxyServer } from '@oxy.so/core/server';
import { kaanaForOxyService } from '../server.js';
import { Kaana } from '../index.js';
import {
  FakeKaana,
  MODEL,
  json,
  errorResponse,
  sessionConfig,
  sse,
  AT,
  LATER,
} from './fakeKaana.js';

const token = `e30.${Buffer.from(JSON.stringify({ iat: Math.floor(Date.now() / 1000), exp: Math.floor(Date.now() / 1000) + 120, authority: 'first_party' })).toString('base64url')}.fixture`;
let fake: FakeKaana;
let authority: Server<unknown>;
let oxy: OxyServer;
let client: ReturnType<typeof kaanaForOxyService>;
let mints: Array<Record<string, unknown>> = [];
let refuse = false;
let hold: Promise<void> | undefined;
let release: (() => void) | undefined;
let credentialReads = 0;
beforeAll(() => {
  fake = new FakeKaana(token).start();
  authority = Bun.serve({
    port: 0,
    hostname: '127.0.0.1',
    async fetch(request) {
      if (new URL(request.url).pathname.endsWith('/challenge')) {
        await hold;
        return Response.json({ nonce: 'fixture' });
      }
      mints.push((await request.json()) as Record<string, unknown>);
      return refuse ? Response.json({}, { status: 403 }) : Response.json({ token, expiresIn: 120 });
    },
  });
});
beforeEach(() => {
  mints = [];
  refuse = false;
  hold = undefined;
  release = undefined;
  credentialReads = 0;
  fake.requests.length = 0;
  fake.frames.length = 0;
  fake.handlers.clear();
  fake.dropAfter = undefined;
  oxy = new OxyServer({
    baseURL: authority.url.origin,
    workloadCredentials: async () => ({
      AccessKeyId: 'ASIA_TEST',
      SecretAccessKey: 'test-only',
      Token: 'temporary-test',
    }),
  });
  client = kaanaForOxyService(
    {
      createServiceClient(options) {
        const shared = oxy.createServiceClient(options);
        return {
          ...shared,
          getAccessToken(target, options) {
            credentialReads++;
            return shared.getAccessToken(target, options);
          },
        };
      },
    },
    { baseURL: fake.baseURL, attribution: { accountId: 'original-person' }, maxRetries: 1 },
  );
});
afterEach(() => {
  release?.();
  client.dispose();
  oxy.dispose();
});
afterAll(() => {
  fake.stop();
  authority.stop(true);
});
const open = {
  model: 'openai/gpt-realtime',
  type: 'session.open',
  kind: 'conversation',
  config: sessionConfig,
} as const;

describe('own Oxy workload transport', () => {
  it('uses one real v2 mint across HTTP and realtime and keeps the exact credential and account on resume', async () => {
    await client.responses.create({
      model: MODEL,
      input: [{ role: 'user', content: [{ type: 'text', text: 'Hello' }] }],
    });
    const session = await client.realtime.connect(open as never);
    expect(fake.sessions.get(session.sessionId)?.socket?.data.user).toBe('original-person');
    await session.resume();
    expect(fake.sessions.get(session.sessionId)?.socket?.data.user).toBe('original-person');
    await session.close();
    expect(credentialReads).toBe(1);
    expect(mints).toHaveLength(1);
    expect(mints[0]).toMatchObject({
      protocolVersion: 2,
      audience: 'kaana.ai',
      provider: 'aws-iam',
    });
    expect(mints[0]).not.toHaveProperty('scopes');
    expect(fake.requests[0].headers.get('x-oxy-user-id')).toBe('original-person');
    expect(fake.requests[0].headers.get('authorization')).toBe(`Bearer ${token}`);
  });
  it('preserves the idempotency key and fixed attribution when Kaana retries before output', async () => {
    let attempts = 0;
    fake.on('POST /responses', () => {
      attempts++;
      if (attempts === 1)
        return errorResponse('service_unavailable', { retryable: true, retryAfterMs: 1 });
      return json('req_test', {
        schemaVersion: 1,
        requestId: 'req_test',
        model: MODEL,
        finishReason: 'stop',
        output: [{ role: 'assistant', content: [{ type: 'text', text: 'Hola.' }] }],
        usage: [{ unit: 'output_tokens', quantity: 2 }],
        routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
        latencyMs: 10,
      });
    });
    const response = await client.responses.create(
      { model: MODEL, input: [{ role: 'user', content: [{ type: 'text', text: 'Hello' }] }] },
      { idempotencyKey: 'original-key' },
    );
    expect(response.finishReason).toBe('stop');
    expect(attempts).toBe(2);
    const sent = fake.requests.filter((request) => request.path === '/responses');
    expect(sent.map((request) => request.headers.get('Idempotency-Key'))).toEqual([
      'original-key',
      'original-key',
    ]);
    expect(
      sent.every((request) => request.headers.get('x-oxy-user-id') === 'original-person'),
    ).toBe(true);
  });
  it('snapshots attribution and retains streaming responses on the shared transport', async () => {
    const attribution = { accountId: 'original-person' };
    client.dispose();
    client = kaanaForOxyService(oxy, { baseURL: fake.baseURL, attribution });
    attribution.accountId = 'mutated-person';
    fake.on('POST /responses', () =>
      sse(
        'req_stream',
        [
          {
            event: 'start',
            data: {
              schemaVersion: 1,
              requestId: 'req_stream',
              sequence: 0,
              type: 'start',
              model: MODEL,
              startedAt: AT,
              generationId: 'gen_1',
            },
          },
          {
            event: 'delta',
            data: {
              schemaVersion: 1,
              requestId: 'req_stream',
              sequence: 1,
              type: 'delta',
              outputIndex: 0,
              channel: 'output_text',
              text: 'Hello',
            },
          },
          {
            event: 'done',
            data: {
              schemaVersion: 1,
              requestId: 'req_stream',
              sequence: 2,
              type: 'done',
              finishReason: 'stop',
              completedAt: LATER,
            },
          },
        ],
        { 'Kaana-Routing-Policy-Id': 'rp_1', 'Kaana-Routing-Policy-Version': '3' },
      ),
    );
    const stream = client.responses.stream({
      model: MODEL,
      input: [{ role: 'user', content: [{ type: 'text', text: 'Hello' }] }],
    });
    const events = [];
    for await (const event of stream) events.push(event);
    expect(events.length).toBeGreaterThan(0);
    expect((await stream.final).finishReason).toBe('stop');
    expect(fake.requests[0].headers.get('x-oxy-user-id')).toBe('original-person');
  });
  it('refuses attribution drift before dispatching either HTTP or a socket', async () => {
    await expect(client.models.list({ onBehalfOfUserId: 'other' })).rejects.toThrow(
      'attribution differs',
    );
    await expect(
      client.realtime.connect(open as never, { onBehalfOfUserId: 'other' }),
    ).rejects.toThrow('attribution differs');
    expect(mints).toHaveLength(0);
    expect(fake.requests).toHaveLength(0);
    expect(fake.frames).toHaveLength(0);
  });
  it('never dispatches when workload attestation is refused', async () => {
    refuse = true;
    await expect(client.models.list()).rejects.toThrow();
    await expect(client.realtime.connect(open as never)).rejects.toThrow();
    expect(fake.requests).toHaveLength(0);
    expect(fake.frames).toHaveLength(0);
  });
  it('cancels before opening a socket while attestation is still pending', async () => {
    hold = new Promise<void>((resolve) => {
      release = resolve;
    });
    const controller = new AbortController();
    const operation = client.realtime.connect(open as never, { signal: controller.signal });
    controller.abort();
    await expect(operation).rejects.toThrow();
    expect(fake.frames).toHaveLength(0);
  });
  it('refuses foreign destinations before constructing the shared transport', () => {
    let constructed = false;
    expect(() =>
      kaanaForOxyService(
        {
          createServiceClient() {
            constructed = true;
            throw Error('unexpected');
          },
        },
        { baseURL: 'https://other.example/v1' },
      ),
    ).toThrow('destination');
    expect(constructed).toBe(false);
  });
  it('does not follow an authenticated HTTP redirect to another origin', async () => {
    let leaked = 0;
    const other = Bun.serve({
      port: 0,
      hostname: '127.0.0.1',
      fetch() {
        leaked++;
        return Response.json({});
      },
    });
    fake.on('POST /responses', () => Response.redirect(other.url, 307));
    try {
      await expect(
        client.responses.create({
          model: MODEL,
          input: [{ role: 'user', content: [{ type: 'text', text: 'Hello' }] }],
        }),
      ).rejects.toThrow();
      expect(leaked).toBe(0);
    } finally {
      other.stop(true);
    }
  });
  it('refuses new HTTP and websocket operations after the shared client is disposed', async () => {
    client.dispose();
    await expect(client.models.list()).rejects.toThrow();
    await expect(client.realtime.connect(open as never)).rejects.toThrow();
    expect(mints).toHaveLength(0);
    expect(fake.frames).toHaveLength(0);
  });
  it('keeps external credential clients separate from authenticated transport', () => {
    expect(
      () =>
        new Kaana({
          credential: 'external',
          transport: oxy.createServiceClient({ baseURL: fake.baseURL, audience: 'kaana.ai' }),
        } as never),
    ).toThrow('cannot be combined');
  });
});
