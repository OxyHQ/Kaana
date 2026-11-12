import { describe, expect, it } from 'bun:test';
import {
  anthropicErrorSchema,
  anthropicMessageSchema,
  anthropicMessagesRequestSchema,
  anthropicStreamEventSchema,
} from '../index.js';

const request = {
  model: 'anthropic/claude-sonnet',
  max_tokens: 1024,
  system: [{ type: 'text', text: 'Be brief.' }],
  messages: [
    { role: 'user', content: 'Weather in Madrid?' },
    {
      role: 'assistant',
      content: [
        { type: 'text', text: 'Let me look.' },
        { type: 'tool_use', id: 'toolu_1', name: 'weather', input: { city: 'Madrid' } },
      ],
    },
    {
      role: 'user',
      content: [{ type: 'tool_result', tool_use_id: 'toolu_1', content: '21C', is_error: false }],
    },
  ],
  tools: [{ name: 'weather', description: 'Weather.', input_schema: { type: 'object' } }],
  tool_choice: { type: 'tool', name: 'weather' },
  temperature: 0.2,
  top_p: 0.9,
  top_k: 40,
  stop_sequences: ['END'],
  stream: true,
  thinking: { type: 'disabled' },
  output_config: { effort: 'high' },
  metadata: { user_id: 'someone' },
};

const textWith = (extra: object) => ({
  role: 'user',
  content: [{ type: 'text', text: 'x', ...extra }],
});

describe('POST /v1/messages request', () => {
  it('accepts a request that populates every member', () => {
    expect(anthropicMessagesRequestSchema.safeParse(request).success).toBe(true);
  });

  it('requires max_tokens, as Anthropic does', () => {
    const { max_tokens: _omitted, ...without } = request;
    expect(anthropicMessagesRequestSchema.safeParse(without).success).toBe(false);
  });

  // The control above proves the schema accepts the shape; each case below
  // changes ONE thing, so a refusal is that thing's.
  it.each([
    [
      'cache_control on a block',
      { messages: [textWith({ cache_control: { type: 'ephemeral' } })] },
    ],
    [
      'cache_control on system',
      { system: [{ type: 'text', text: 'x', cache_control: { type: 'ephemeral' } }] },
    ],
    [
      'cache_control on a tool',
      { tools: [{ name: 'a', input_schema: {}, cache_control: { type: 'ephemeral' } }] },
    ],
    ['cache_control on the request', { cache_control: { type: 'ephemeral' } }],
    ['thinking enabled', { thinking: { type: 'enabled', budget_tokens: 2048 } }],
    ['effort beyond high', { output_config: { effort: 'max' } }],
    ['a server tool', { tools: [{ type: 'web_search_20250305', name: 'web_search' }] }],
    [
      'a thinking block',
      {
        messages: [
          { role: 'assistant', content: [{ type: 'thinking', thinking: 'x', signature: 's' }] },
        ],
      },
    ],
    [
      'a tool_use block from the user',
      {
        messages: [
          { role: 'user', content: [{ type: 'tool_use', id: 'a', name: 'b', input: {} }] },
        ],
      },
    ],
    [
      'a tool_result block from the assistant',
      {
        messages: [{ role: 'assistant', content: [{ type: 'tool_result', tool_use_id: 'a' }] }],
      },
    ],
    ['a tool choice without tools', { tools: undefined }],
    ['an unknown member', { service_tier: 'auto' }],
    [
      'more than eight stop sequences',
      { stop_sequences: Array.from({ length: 9 }, (_, i) => `s${i}`) },
    ],
  ])('refuses %s', (_name, change) => {
    expect(anthropicMessagesRequestSchema.safeParse({ ...request, ...change }).success).toBe(false);
  });

  it('parses an image url source so the server can refuse it by name', () => {
    const parsed = anthropicMessagesRequestSchema.safeParse({
      ...request,
      tool_choice: undefined,
      tools: undefined,
      messages: [
        {
          role: 'user',
          content: [{ type: 'image', source: { type: 'url', url: 'https://x.test/a.png' } }],
        },
      ],
    });
    expect(parsed.success).toBe(true);
  });
});

const model = 'anthropic/claude-sonnet@2026-05-01';
const usage = { input_tokens: 10, output_tokens: 4, cache_read_input_tokens: 6 };

describe('POST /v1/messages response', () => {
  it('accepts a message with text and a tool_use block and refuses what leaks', () => {
    const body = {
      id: 'msg_1',
      type: 'message',
      role: 'assistant',
      model,
      content: [
        { type: 'text', text: 'Looking.' },
        { type: 'tool_use', id: 'toolu_1', name: 'weather', input: { city: 'Madrid' } },
      ],
      stop_reason: 'tool_use',
      stop_sequence: null,
      usage,
    };
    expect(anthropicMessageSchema.safeParse(body).success).toBe(true);
    expect(
      anthropicMessageSchema.safeParse({ ...body, stop_reason: 'stop_sequence' }).success,
    ).toBe(false);
    expect(anthropicMessageSchema.safeParse({ ...body, deploymentId: 'dep_1' }).success).toBe(
      false,
    );
    expect(
      anthropicMessageSchema.safeParse({
        ...body,
        usage: { ...usage, cache_creation_input_tokens: 1 },
      }).success,
    ).toBe(false);
  });
});

describe('POST /v1/messages stream and errors', () => {
  const start = {
    type: 'message_start',
    message: {
      id: 'msg_1',
      type: 'message',
      role: 'assistant',
      model,
      content: [],
      stop_reason: null,
      stop_sequence: null,
      usage: { input_tokens: 0, output_tokens: 0 },
    },
  };
  it.each([
    ['message_start', start],
    [
      'content_block_start text',
      { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } },
    ],
    [
      'content_block_start tool_use',
      {
        type: 'content_block_start',
        index: 1,
        content_block: { type: 'tool_use', id: 'toolu_1', name: 'weather', input: {} },
      },
    ],
    [
      'text_delta',
      { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'Hi' } },
    ],
    [
      'input_json_delta',
      {
        type: 'content_block_delta',
        index: 1,
        delta: { type: 'input_json_delta', partial_json: '{"a"' },
      },
    ],
    ['content_block_stop', { type: 'content_block_stop', index: 0 }],
    [
      'message_delta',
      { type: 'message_delta', delta: { stop_reason: 'end_turn', stop_sequence: null }, usage },
    ],
    ['message_stop', { type: 'message_stop' }],
    ['ping', { type: 'ping' }],
    [
      'error',
      {
        type: 'error',
        error: { type: 'overloaded_error', message: 'Busy.' },
        request_id: 'req_1',
      },
    ],
  ])('parses a %s frame', (_name, frame) => {
    expect(anthropicStreamEventSchema.safeParse(frame).success).toBe(true);
  });

  it('refuses a frame that carries a provider member', () => {
    expect(anthropicStreamEventSchema.safeParse({ type: 'ping', provider: 'x' }).success).toBe(
      false,
    );
  });

  it('refuses an error type Anthropic does not have', () => {
    expect(
      anthropicErrorSchema.safeParse({
        type: 'error',
        error: { type: 'provider_error', message: 'x' },
      }).success,
    ).toBe(false);
  });
});
