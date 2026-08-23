/**
 * The envelope builder, and the mistakes it exists to make impossible.
 *
 * Each was reachable by hand and none errors where it is made: a `text` input
 * for a chat turn becomes a confusing upstream refusal, a malformed
 * `routingPolicy` is accepted and served because `Request.Validate` deliberately
 * checks nothing about it, and a missing `apiFormat` fails at the far end of the
 * wire.
 */

import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { ZodError } from 'zod';

import { buildInferenceRequest, serializeInferenceRequest } from '../src/envelope.js';
import { KaanaEnvelopeError } from '../src/errors.js';
import { chatEnvelope } from './fixtures.js';

const BASE = {
  attribution: {
    accountId: 'acc_01JQZ',
    applicationId: 'app_01JQZ',
    credentialId: 'cred_01JQZ',
    environment: 'production',
    requestId: 'req_01JQZABCDEF',
  },
  target: { kind: 'model', modelReference: 'openai/gpt-4o-mini' },
  stream: true,
  client: { apiFormat: 'chat_completions', endpoint: '/v1/chat/completions' },
  routingPolicy: { routingPolicyId: 'rp_01JQZ', policyVersion: 3 },
} as const;

describe('buildInferenceRequest', () => {
  it('normalizes a chat turn to the messages input, not the embedding one', () => {
    const request = chatEnvelope();

    assert.equal(request.input.format, 'messages');
    assert.deepEqual(request.input, {
      format: 'messages',
      messages: [{ role: 'user', content: [{ type: 'text', text: 'hi' }] }],
    });
  });

  it('routes a batch of strings to text_batch and a single string to text', () => {
    const batch = buildInferenceRequest({ ...BASE, input: { texts: ['a', 'b'] } });
    const single = buildInferenceRequest({ ...BASE, input: { text: 'a' } });

    assert.deepEqual(batch.input, { format: 'text_batch', texts: ['a', 'b'] });
    assert.deepEqual(single.input, { format: 'text', text: 'a' });
  });

  it('fills in the properties the contract requires and callers forget', () => {
    const request = buildInferenceRequest({
      ...BASE,
      input: { messages: [{ role: 'user', content: 'hi' }] },
    });

    assert.equal(request.schemaVersion, 1);
    assert.equal(request.modality, 'text');
    assert.deepEqual(request.sampling, {});
    assert.deepEqual(request.tools, []);
    assert.deepEqual(request.attribution.principal.inferenceScopes, ['inference:invoke']);
    assert.match(request.client.receivedAt, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
  });

  it('keeps content parts a caller supplied verbatim', () => {
    const request = buildInferenceRequest({
      ...BASE,
      input: {
        messages: [
          {
            role: 'user',
            content: [
              { type: 'text', text: 'describe this' },
              {
                type: 'image',
                source: { kind: 'url', url: 'https://example.test/a.png' },
                detail: 'high',
              },
            ],
          },
        ],
      },
      modality: 'image',
    });

    // Well-formed and, today, unservable: openaicompat refuses a non-text
    // modality. The builder models the contract, not one adapter set.
    assert.equal(request.modality, 'image');
    assert.deepEqual(request.input, {
      format: 'messages',
      messages: [
        {
          role: 'user',
          content: [
            { type: 'text', text: 'describe this' },
            {
              type: 'image',
              source: { kind: 'url', url: 'https://example.test/a.png' },
              detail: 'high',
            },
          ],
        },
      ],
    });
  });

  it('refuses a routing policy reference the data plane would accept unvalidated', () => {
    assert.throws(
      () =>
        buildInferenceRequest({
          ...BASE,
          input: { messages: [{ role: 'user', content: 'hi' }] },
          routingPolicy: { routingPolicyId: 'rp_01JQZ', policyVersion: 0 },
        }),
      KaanaEnvelopeError,
    );
    assert.throws(
      () =>
        buildInferenceRequest({
          ...BASE,
          input: { messages: [{ role: 'user', content: 'hi' }] },
          routingPolicy: { routingPolicyId: '', policyVersion: 3 },
        }),
      KaanaEnvelopeError,
    );
  });

  it('refuses an empty conversation', () => {
    assert.throws(
      () => buildInferenceRequest({ ...BASE, input: { messages: [] } }),
      KaanaEnvelopeError,
    );
  });

  it('refuses a tool choice with no tools, and duplicate tool names', () => {
    const tool = { type: 'function', name: 'lookup', parameters: { type: 'object' } } as const;

    assert.throws(
      () =>
        buildInferenceRequest({
          ...BASE,
          input: { messages: [{ role: 'user', content: 'hi' }] },
          toolChoice: 'required',
        }),
      KaanaEnvelopeError,
    );
    assert.throws(
      () =>
        buildInferenceRequest({
          ...BASE,
          input: { messages: [{ role: 'user', content: 'hi' }] },
          tools: [tool, { ...tool, description: 'a second definition of one name' }],
        }),
      KaanaEnvelopeError,
    );
  });

  it('refuses a model reference that is not <publisher>/<model>', () => {
    assert.throws(
      () =>
        buildInferenceRequest({
          ...BASE,
          target: { kind: 'model', modelReference: 'gpt-4o-mini' },
          input: { messages: [{ role: 'user', content: 'hi' }] },
        }),
      KaanaEnvelopeError,
    );
  });

  it('accepts a pinned revision, which is served or refused but never substituted', () => {
    const request = buildInferenceRequest({
      ...BASE,
      target: { kind: 'model', modelReference: 'openai/gpt-4o-mini@2026-05-01' },
      input: { messages: [{ role: 'user', content: 'hi' }] },
    });

    assert.deepEqual(request.target, {
      kind: 'model',
      modelReference: 'openai/gpt-4o-mini@2026-05-01',
    });
  });

  it('refuses a tool message that names no tool call', () => {
    assert.throws(
      () => buildInferenceRequest({ ...BASE, input: { messages: [{ role: 'tool', content: '42' }] } }),
      KaanaEnvelopeError,
    );
  });

  it('names the failing field on the ZodError it carries as a cause', () => {
    let cause: unknown;
    try {
      buildInferenceRequest({ ...BASE, input: { messages: [] } });
    } catch (error) {
      cause = (error as KaanaEnvelopeError).cause;
    }

    assert.ok(cause instanceof ZodError);
    // Computed unconditionally: wrapping the assertion in an `instanceof` guard
    // would let a non-ZodError cause pass by skipping it.
    const paths = cause instanceof ZodError ? cause.issues.map((issue) => issue.path.join('.')) : [];
    assert.ok(paths.some((path) => path.includes('messages')));
  });
});

describe('serializeInferenceRequest', () => {
  it('renders bytes that parse back to the envelope', () => {
    const request = chatEnvelope();

    const bytes = serializeInferenceRequest(request);

    assert.deepEqual(
      JSON.parse(new TextDecoder().decode(bytes)),
      JSON.parse(JSON.stringify(request)),
    );
  });

  it('omits the optional properties the caller left unset', () => {
    const request = chatEnvelope();

    const decoded = JSON.parse(new TextDecoder().decode(serializeInferenceRequest(request))) as
      Record<string, unknown>;

    assert.equal(Object.keys(decoded).includes('idempotencyKey'), false);
    assert.equal(Object.keys(decoded).includes('toolChoice'), false);
    assert.equal(Object.keys(decoded).includes('responseFormat'), false);
    assert.equal(
      Object.keys(decoded['attribution'] as Record<string, unknown>).includes('userId'),
      false,
    );
  });

  it('renders compact UTF-8 JSON with nothing prepended', () => {
    const request = chatEnvelope();

    const bytes = serializeInferenceRequest(request);

    // No pretty printing and no byte-order mark: edgeauth hashes these exact
    // bytes, so anything the encoder adds has to be deliberate.
    assert.equal(new TextDecoder().decode(bytes), JSON.stringify(request));
    assert.equal(bytes[0], '{'.charCodeAt(0));
  });
});
