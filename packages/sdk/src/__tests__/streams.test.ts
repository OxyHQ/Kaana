import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { responsesRequestSchema } from '@kaana.ai/contracts';
import {
  Kaana,
  KaanaCancelledError,
  KaanaConnectionError,
  KaanaProtocolError,
  KaanaUnavailableError,
} from '../index.js';
import {
  AT,
  CREDENTIAL,
  FakeKaana,
  LATER,
  MODEL,
  nextRequestId,
  publicError,
  sse,
} from './fakeKaana.js';

let fake: FakeKaana;
let kaana: Kaana;

beforeAll(() => {
  fake = new FakeKaana().start();
});
afterAll(() => fake.stop());
beforeEach(() => {
  fake.handlers.clear();
  fake.requests.length = 0;
  kaana = new Kaana({ credential: CREDENTIAL, baseURL: fake.baseURL });
});

const policyHeaders = { 'Kaana-Routing-Policy-Id': 'rp_1', 'Kaana-Routing-Policy-Version': '3' };

function event(requestId: string, sequence: number, body: Record<string, unknown>) {
  return { event: String(body.type), data: { schemaVersion: 1, requestId, sequence, ...body } };
}

function responseFrames(requestId: string, terminal: 'done' | 'error' | 'none') {
  const frames = [
    event(requestId, 0, { type: 'start', model: MODEL, startedAt: AT, generationId: 'gen_1' }),
    event(requestId, 1, { type: 'delta', outputIndex: 0, channel: 'output_text', text: 'Ho' }),
    event(requestId, 2, { type: 'delta', outputIndex: 0, channel: 'reasoning', text: 'hmm' }),
    event(requestId, 3, { type: 'delta', outputIndex: 0, channel: 'output_text', text: 'la.' }),
    event(requestId, 4, {
      type: 'tool_call',
      toolCallId: 'call_1',
      name: 'lookup',
      argumentsDelta: '{"q":',
      complete: false,
    }),
    event(requestId, 5, {
      type: 'tool_call',
      toolCallId: 'call_1',
      argumentsDelta: '1}',
      complete: true,
    }),
    event(requestId, 6, {
      type: 'usage',
      units: [{ unit: 'output_tokens', quantity: 7 }],
      usageSource: 'provider_reported',
    }),
  ];
  if (terminal === 'done') {
    frames.push(
      event(requestId, 7, { type: 'done', finishReason: 'tool_calls', completedAt: LATER }),
    );
  } else if (terminal === 'error') {
    frames.push(
      event(requestId, 7, {
        type: 'error',
        error: publicError('provider_timeout', { retryable: true, requestId }),
      }),
    );
  }
  return frames;
}

describe('responses.stream', () => {
  it('yields validated events and folds them into the response', async () => {
    fake.on('POST /responses', (_request, recorded) => {
      expect(responsesRequestSchema.parse(recorded.json).stream).toBe(true);
      expect(recorded.headers.get('Accept')).toBe('text/event-stream');
      const requestId = nextRequestId();
      return sse(requestId, responseFrames(requestId, 'done'), policyHeaders);
    });
    const stream = kaana.responses.stream({ model: MODEL, input: 'Hola' });
    const types: string[] = [];
    for await (const item of stream) types.push(item.type);
    expect(types).toEqual([
      'start',
      'delta',
      'delta',
      'delta',
      'tool_call',
      'tool_call',
      'usage',
      'done',
    ]);
    const response = await stream.final;
    expect(await stream.requestId).toBe(response.requestId);
    expect(response.output).toEqual([
      {
        role: 'assistant',
        content: [{ type: 'text', text: 'Hola.' }],
        toolCalls: [{ id: 'call_1', name: 'lookup', arguments: '{"q":1}' }],
      },
    ]);
    expect(response.usage).toEqual([{ unit: 'output_tokens', quantity: 7 }]);
    expect(response.finishReason).toBe('tool_calls');
    expect(response.latencyMs).toBe(812);
    expect(response.routingPolicy).toEqual({ routingPolicyId: 'rp_1', policyVersion: 3 });
    expect(response.generationId).toBe('gen_1');
  });

  it('drains when only final is awaited', async () => {
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      return sse(requestId, responseFrames(requestId, 'done'), policyHeaders);
    });
    const response = await kaana.responses.stream({ model: MODEL, input: 'Hola' }).final;
    expect(response.output[0].content).toEqual([{ type: 'text', text: 'Hola.' }]);
  });

  it('delivers a mid-stream failure typed, after output', async () => {
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      return sse(requestId, responseFrames(requestId, 'error'), policyHeaders);
    });
    const stream = kaana.responses.stream({ model: MODEL, input: 'Hola' });
    const seen: string[] = [];
    const thrown = await (async () => {
      for await (const item of stream) seen.push(item.type);
    })().catch((e) => e);
    expect(seen.at(-1)).toBe('error');
    expect(thrown).toBeInstanceOf(KaanaUnavailableError);
    expect(thrown.status).toBeNull();
    expect(thrown.afterOutput).toBe(true);
    expect(thrown.code).toBe('provider_timeout');
    await expect(stream.final).rejects.toBe(thrown);
  });

  it('reports a stream cut before its terminal event', async () => {
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      return sse(requestId, responseFrames(requestId, 'none'), policyHeaders);
    });
    await expect(
      kaana.responses.stream({ model: MODEL, input: 'Hola' }).final,
    ).rejects.toBeInstanceOf(KaanaConnectionError);
  });

  it('refuses an event of another request and a sequence that does not advance', async () => {
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      const frames = responseFrames(requestId, 'done');
      frames[2] = event('req_other', 2, {
        type: 'delta',
        outputIndex: 0,
        channel: 'output_text',
        text: 'x',
      });
      return sse(requestId, frames, policyHeaders);
    });
    await expect(
      kaana.responses.stream({ model: MODEL, input: 'Hola' }).final,
    ).rejects.toBeInstanceOf(KaanaProtocolError);
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      const frames = responseFrames(requestId, 'done');
      frames[3] = event(requestId, 1, {
        type: 'delta',
        outputIndex: 0,
        channel: 'output_text',
        text: 'x',
      });
      return sse(requestId, frames, policyHeaders);
    });
    await expect(
      kaana.responses.stream({ model: MODEL, input: 'Hola' }).final,
    ).rejects.toBeInstanceOf(KaanaProtocolError);
  });

  it('cancels when the caller stops iterating', async () => {
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      return sse(requestId, responseFrames(requestId, 'done'), policyHeaders);
    });
    const stream = kaana.responses.stream({ model: MODEL, input: 'Hola' });
    for await (const item of stream) {
      if (item.type === 'delta') break;
    }
    const error = await stream.final.catch((e) => e);
    expect(error).toBeInstanceOf(KaanaCancelledError);
    expect(error.afterOutput).toBe(true);
  });

  it('is consumed once', async () => {
    fake.on('POST /responses', () => {
      const requestId = nextRequestId();
      return sse(requestId, responseFrames(requestId, 'done'), policyHeaders);
    });
    const stream = kaana.responses.stream({ model: MODEL, input: 'Hola' });
    await stream.final;
    expect(() => stream[Symbol.asyncIterator]()).toThrow(TypeError);
  });
});

describe('chat.completions.stream', () => {
  const chunk = (
    delta: Record<string, unknown>,
    finish: string | null = null,
    usage?: unknown,
  ) => ({
    data: {
      id: 'chatcmpl-1',
      object: 'chat.completion.chunk',
      created: 1760521260,
      model: MODEL,
      choices: [{ index: 0, delta, finish_reason: finish }],
      ...(usage === undefined ? {} : { usage }),
    },
  });
  const usage = {
    prompt_tokens: 3,
    completion_tokens: 2,
    total_tokens: 5,
    prompt_tokens_details: { cached_tokens: 0 },
    completion_tokens_details: { reasoning_tokens: 0 },
  };

  it('folds OpenAI chunks up to [DONE]', async () => {
    fake.on('POST /chat/completions', () =>
      sse(nextRequestId(), [
        chunk({ role: 'assistant', content: 'Ho' }),
        chunk({ content: 'la' }),
        chunk({
          tool_calls: [
            { index: 0, id: 'call_1', type: 'function', function: { name: 'f', arguments: '{' } },
          ],
        }),
        chunk({ tool_calls: [{ index: 0, function: { arguments: '}' } }] }, 'tool_calls', usage),
        { data: '[DONE]' },
      ]),
    );
    const stream = kaana.chat.completions.stream({
      model: 'instant',
      messages: [{ role: 'user', content: 'Hola' }],
    });
    let chunks = 0;
    for await (const _item of stream) chunks += 1;
    expect(chunks).toBe(4);
    const completion = await stream.final;
    expect(completion.choices[0].message).toEqual({
      role: 'assistant',
      content: 'Hola',
      tool_calls: [{ id: 'call_1', type: 'function', function: { name: 'f', arguments: '{}' } }],
    });
    expect(completion.choices[0].finish_reason).toBe('tool_calls');
    expect(completion.usage.total_tokens).toBe(5);
  });

  it('turns an error frame into a typed failure after output', async () => {
    fake.on('POST /chat/completions', () =>
      sse(nextRequestId(), [
        chunk({ role: 'assistant', content: 'Ho' }),
        {
          data: {
            error: { message: 'boom', type: 'server_error', param: null, code: 'provider_error' },
          },
        },
      ]),
    );
    const error = await kaana.chat.completions
      .stream({ model: 'instant', messages: [{ role: 'user', content: 'Hola' }] })
      .final.catch((e) => e);
    expect(error).toBeInstanceOf(KaanaUnavailableError);
    expect(error.afterOutput).toBe(true);
    expect(error.status).toBeNull();
  });
});
