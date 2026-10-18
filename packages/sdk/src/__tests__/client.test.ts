import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import {
  Kaana,
  KaanaAuthenticationError,
  KaanaBillingError,
  KaanaCancelledError,
  KaanaConnectionError,
  KaanaError,
  KaanaInvalidRequestError,
  KaanaProtocolError,
  KaanaRateLimitError,
  KaanaUnavailableError,
} from '../index.js';
import { CREDENTIAL, errorResponse, FakeKaana, json, MODEL, nextRequestId } from './fakeKaana.js';

const speechModel = {
  schemaVersion: 1,
  modelId: 'elevenlabs/eleven-multilingual',
  publisher: { slug: 'elevenlabs', displayName: 'ElevenLabs' },
  displayName: 'Eleven Multilingual',
  currentRevision: 'v2',
  availableRevisions: ['v2'],
  capabilities: {
    inputModalities: ['text'],
    outputModalities: ['audio'],
    tools: false,
    parallelToolCalls: false,
    structuredOutput: false,
    jsonMode: false,
    reasoning: false,
    reasoningEfforts: [],
    streaming: true,
    promptCaching: false,
    maxContextTokens: 5000,
    maxOutputTokens: 5000,
  },
  license: {
    licenseId: 'elevenlabs-terms',
    displayName: 'ElevenLabs Terms',
    commercialUseAllowed: true,
    requiresAttribution: false,
  },
  provenance: { releaseKind: 'third_party_hosted' },
  regions: [],
  dataPolicy: {
    retainsPayloads: false,
    retentionDays: 0,
    trainsOnCustomerData: false,
    zeroDataRetentionAvailable: true,
    subprocessors: [],
  },
  deprecation: { status: 'active' },
  evaluations: [],
};

const generation = {
  schemaVersion: 1,
  kind: 'charge',
  requestId: 'req_1',
  applicationId: 'app_1',
  credentialId: 'cred_1',
  environment: 'production',
  outcome: 'completed',
  usageSource: 'provider_reported',
  units: [{ unit: 'input_tokens', quantity: 3 }],
  model: MODEL,
  settledAt: '2026-10-15T09:41:03.000Z',
  receiptId: 'rcpt_1',
  priceSnapshot: {
    priceVersionId: 'pv_1',
    currency: 'USD',
    unitPrices: [{ unit: 'input_tokens', amount: '1.25', per: 1000000, currency: 'USD' }],
  },
  billedAmount: '0.000003750000',
  currency: 'USD',
  platformFeeOnly: false,
};

let fake: FakeKaana;
let kaana: Kaana;

beforeAll(() => {
  fake = new FakeKaana().start();
});
afterAll(() => fake.stop());
beforeEach(() => {
  fake.handlers.clear();
  fake.requests.length = 0;
  kaana = new Kaana({ credential: CREDENTIAL, baseURL: fake.baseURL, maxRetries: 2 });
});

describe('construction', () => {
  it('speaks only to kaana.ai/v1 or loopback', () => {
    expect(new Kaana({ credential: 'x' }).baseURL).toBe('https://kaana.ai/v1');
    expect(new Kaana({ credential: 'x', baseURL: 'https://kaana.ai/v1/' }).baseURL).toBe(
      'https://kaana.ai/v1',
    );
    expect(new Kaana({ credential: 'x', baseURL: 'http://localhost:8080/v1' }).baseURL).toBe(
      'http://localhost:8080/v1',
    );
    expect(() => new Kaana({ credential: 'x', baseURL: 'https://api.oxy.so/v1' })).toThrow(
      TypeError,
    );
    expect(() => new Kaana({ credential: 'x', baseURL: 'http://kaana.ai/v1' })).toThrow(TypeError);
    expect(() => new Kaana({ credential: 'x', baseURL: 'https://evil.example/v1' })).toThrow(
      TypeError,
    );
  });
});

describe('credentials and headers', () => {
  it('reads a credential function on every request and never caches it', async () => {
    fake.on('GET /models', () => json(nextRequestId(), { data: [speechModel] }));
    let calls = 0;
    const client = new Kaana({
      credential: async () => {
        calls += 1;
        return CREDENTIAL;
      },
      baseURL: fake.baseURL,
    });
    await client.models.list();
    await client.models.list();
    expect(calls).toBe(2);
  });

  it('sends the idempotency key and delegated user', async () => {
    await kaana.responses.create(
      { model: MODEL, input: 'Hola' },
      { idempotencyKey: 'idem-1', onBehalfOfUserId: 'user_1' },
    );
    const sent = fake.requests[0];
    expect(sent.headers.get('Idempotency-Key')).toBe('idem-1');
    expect(sent.headers.get('X-Oxy-User-Id')).toBe('user_1');
    expect(sent.headers.get('Content-Type')).toBe('application/json');
  });

  it('refuses an empty credential before any request', async () => {
    const client = new Kaana({ credential: () => '', baseURL: fake.baseURL });
    await expect(client.models.list()).rejects.toBeInstanceOf(KaanaAuthenticationError);
    expect(fake.requests.length).toBe(0);
  });
});

describe('catalogue', () => {
  it('lists, reads one model by reference and lists power levels', async () => {
    fake.on('GET /models', () => json(nextRequestId(), { data: [speechModel] }));
    fake.on('GET /models/*', (_request, recorded) => {
      expect(recorded.path).toBe('/models/elevenlabs/eleven-multilingual%40v2');
      return json(nextRequestId(), speechModel);
    });
    fake.on('GET /power-levels', () =>
      json(nextRequestId(), {
        data: [{ level: 'auto', modelLines: ['openai/gpt-5'] }],
      }),
    );
    expect((await kaana.models.list())[0].modelId).toBe('elevenlabs/eleven-multilingual');
    expect((await kaana.models.get('elevenlabs/eleven-multilingual@v2')).currentRevision).toBe(
      'v2',
    );
    expect(await kaana.powerLevels.list()).toEqual([
      { level: 'auto', modelLines: ['openai/gpt-5'] },
    ]);
  });

  it('refuses a model reference that is not one', async () => {
    await expect(kaana.models.get('gpt-4o')).rejects.toBeInstanceOf(KaanaInvalidRequestError);
  });
});

describe('typed errors', () => {
  it('maps a refusal to its class with every field', async () => {
    fake.on('POST /responses', () => errorResponse('insufficient_balance', { param: 'model' }));
    const error = await kaana.responses.create({ model: MODEL, input: 'Hola' }).catch((e) => e);
    expect(error).toBeInstanceOf(KaanaBillingError);
    expect(error).toBeInstanceOf(KaanaError);
    expect(error.code).toBe('insufficient_balance');
    expect(error.status).toBe(402);
    expect(error.retryable).toBe(false);
    expect(error.param).toBe('model');
    expect(error.afterOutput).toBe(false);
    expect(error.requestId).toStartWith('req_fake_');
    expect(fake.requests.length).toBe(1);
  });

  it('reads the chat dialect error body', async () => {
    fake.on('POST /chat/completions', () =>
      json(
        nextRequestId(),
        { error: { message: 'slow down', type: 'rate_limit', param: null, code: 'rate_limited' } },
        429,
        { 'Retry-After': '0' },
      ),
    );
    const client = new Kaana({ credential: CREDENTIAL, baseURL: fake.baseURL, maxRetries: 0 });
    const error = await client.chat.completions
      .create({ model: 'instant', messages: [{ role: 'user', content: 'Hola' }] })
      .catch((e) => e);
    expect(error).toBeInstanceOf(KaanaRateLimitError);
    expect(error.retryable).toBe(true);
    expect(error.retryAfterMs).toBe(0);
  });

  it('refuses what the contract refuses before sending', async () => {
    await expect(
      kaana.responses.create({ model: MODEL, input: 'Hola', logprobs: true } as never),
    ).rejects.toBeInstanceOf(KaanaInvalidRequestError);
    expect(fake.requests.length).toBe(0);
  });

  it('treats a body outside the contract as a protocol error', async () => {
    fake.on('POST /embeddings', () => json(nextRequestId(), { object: 'list', data: [] }));
    const error = await kaana.embeddings
      .create({ model: 'openai/text-embedding-3-small', input: 'hola' })
      .catch((e) => e);
    expect(error).toBeInstanceOf(KaanaProtocolError);
    expect(fake.requests.length).toBe(1);
  });

  it('requires Kaana-Request-Id', async () => {
    fake.on(
      'GET /power-levels',
      () =>
        new Response(JSON.stringify({ data: [] }), {
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    await expect(kaana.powerLevels.list()).rejects.toBeInstanceOf(KaanaProtocolError);
  });
});

describe('retries', () => {
  it('retries a retryable refusal, honouring retryAfterMs, with the same idempotency key', async () => {
    let attempts = 0;
    fake.on('POST /responses', (request) => {
      attempts += 1;
      expect(request.headers.get('Idempotency-Key')).toBe('idem-retry');
      if (attempts < 3)
        return errorResponse('service_unavailable', { retryable: true, retryAfterMs: 5 });
      return json(nextRequestId(), {
        schemaVersion: 1,
        requestId: 'req_ok',
        model: MODEL,
        finishReason: 'stop',
        output: [],
        usage: [],
        routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
        latencyMs: 1,
      });
    });
    const response = await kaana.responses.create(
      { model: MODEL, input: 'Hola' },
      { idempotencyKey: 'idem-retry' },
    );
    expect(response.requestId).toBe('req_ok');
    expect(attempts).toBe(3);
  });

  it('gives up after maxRetries', async () => {
    let attempts = 0;
    fake.on('POST /responses', () => {
      attempts += 1;
      return errorResponse('provider_overloaded', { retryable: true, retryAfterMs: 1 });
    });
    await expect(kaana.responses.create({ model: MODEL, input: 'Hola' })).rejects.toBeInstanceOf(
      KaanaUnavailableError,
    );
    expect(attempts).toBe(3);
  });

  it('never retries a non-retryable code', async () => {
    let attempts = 0;
    fake.on('POST /responses', () => {
      attempts += 1;
      return errorResponse('model_not_found');
    });
    await expect(kaana.responses.create({ model: MODEL, input: 'Hola' })).rejects.toBeInstanceOf(
      KaanaInvalidRequestError,
    );
    expect(attempts).toBe(1);
  });

  it('treats a gateway answer without the contract body as a retryable outage', async () => {
    let attempts = 0;
    fake.on('GET /power-levels', () => {
      attempts += 1;
      return new Response('<html>bad gateway</html>', { status: 502 });
    });
    const client = new Kaana({ credential: CREDENTIAL, baseURL: fake.baseURL, maxRetries: 1 });
    const error = await client.powerLevels.list().catch((e) => e);
    expect(error).toBeInstanceOf(KaanaUnavailableError);
    expect(error.code).toBe('service_unavailable');
    expect(attempts).toBe(2);
  });

  it('retries a network failure, then reports it as a connection error', async () => {
    let attempts = 0;
    const client = new Kaana({
      credential: CREDENTIAL,
      maxRetries: 1,
      fetch: async () => {
        attempts += 1;
        throw new TypeError('fetch failed');
      },
    });
    const error = await client.powerLevels.list().catch((e) => e);
    expect(error).toBeInstanceOf(KaanaConnectionError);
    expect(error.timedOut).toBe(false);
    expect(attempts).toBe(2);
  });

  it('times out without retrying', async () => {
    fake.on('GET /power-levels', async () => {
      await Bun.sleep(200);
      return json(nextRequestId(), { data: [] });
    });
    const error = await kaana.powerLevels.list({ timeoutMs: 20 }).catch((e) => e);
    expect(error).toBeInstanceOf(KaanaConnectionError);
    expect(error.timedOut).toBe(true);
    expect(fake.requests.length).toBe(1);
  });

  it('reports a caller abort as cancelled', async () => {
    fake.on('GET /power-levels', async () => {
      await Bun.sleep(200);
      return json(nextRequestId(), { data: [] });
    });
    const controller = new AbortController();
    setTimeout(() => controller.abort(), 10);
    await expect(kaana.powerLevels.list({ signal: controller.signal })).rejects.toBeInstanceOf(
      KaanaCancelledError,
    );
  });
});

describe('the other endpoints', () => {
  it('serves chat completions, embeddings, images and decisions', async () => {
    const chat = await kaana.chat.completions.create({
      model: 'instant',
      messages: [{ role: 'user', content: 'Hola' }],
    });
    expect(chat.choices[0].message.content).toBe('Hola.');
    const embeddings = await kaana.embeddings.create({
      model: 'openai/text-embedding-3-small',
      input: ['hola'],
    });
    expect(embeddings.data[0].embedding).toEqual([0.1, 0.2]);
    const images = await kaana.images.generate({ model: 'openai/gpt-image-1', prompt: 'A fox' });
    expect(images.data.length).toBe(1);
    const decision = await kaana.decisions.create({
      model: MODEL,
      state: 'A post about cats.',
      questions: [{ id: 'q1', kind: 'choice', question: 'Topic?', options: ['pets', 'cars'] }],
    });
    expect(decision.data[0].id).toBe('q1');
  });

  it('refuses decisions that do not answer the request it sent', async () => {
    const question = {
      id: 'q1',
      kind: 'choice' as const,
      question: 'Topic?',
      options: ['pets', 'cars'],
    };
    // Unpinned: the profile chooses, so any revision may answer.
    const chosen = await kaana.decisions.create({ state: 'Cats.', questions: [question] });
    expect(chosen.model).toBe(MODEL);
    for (const request of [
      { state: 'Cats.', questions: [{ ...question, options: ['cars', 'pets', 'boats'] }] },
      { model: 'openai/gpt-6-luna@observed-2026-10-01', state: 'Cats.', questions: [question] },
    ]) {
      await expect(kaana.decisions.create(request)).rejects.toBeInstanceOf(KaanaProtocolError);
    }
  });

  it('reads generation records by id and by idempotency key', async () => {
    fake.on('GET /generations/*', (_request, recorded) => {
      expect(recorded.path).toBe('/generations/gen_1');
      return json(nextRequestId(), generation);
    });
    fake.on('GET /generations', (_request, recorded) => {
      expect(recorded.search.get('idempotency_key')).toBe('idem 1');
      return json(nextRequestId(), generation);
    });
    expect((await kaana.generations.get('gen_1')).kind).toBe('charge');
    expect((await kaana.generations.getByIdempotencyKey('idem 1')).requestId).toBe('req_1');
  });
});
