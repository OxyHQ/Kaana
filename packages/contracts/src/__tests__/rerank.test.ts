import { describe, expect, it } from 'bun:test';
import { MAX_RERANK_DOCUMENTS, rerankRequestSchema, rerankResponseSchema } from '../index.js';
import { grantOperationSchema, inferenceRequestSchema } from '../internal.js';

const valid = { model: 'cohere/rerank-v3', query: 'q', documents: ['a', 'b'] };
const ok = (value: unknown) => rerankRequestSchema.safeParse(value).success;

describe('the rerank grant operation', () => {
  it('is the last member of the operation list', () => {
    expect(grantOperationSchema.options.at(-1)).toBe('rerank');
  });
});

describe('POST /v1/rerank request', () => {
  it('accepts a valid request, with and without the optional members', () => {
    expect(ok(valid)).toBe(true);
    expect(
      ok({ ...valid, model: 'cohere/rerank-v3@2024-01', top_n: 1, return_documents: true }),
    ).toBe(true);
  });

  it('refuses an empty query, no or empty documents, too many, and non-strings', () => {
    expect(ok({ ...valid, query: '' })).toBe(false);
    expect(ok({ ...valid, documents: [] })).toBe(false);
    expect(ok({ ...valid, documents: [''] })).toBe(false);
    expect(ok({ ...valid, documents: Array(MAX_RERANK_DOCUMENTS + 1).fill('x') })).toBe(false);
    expect(ok({ ...valid, documents: Array(MAX_RERANK_DOCUMENTS).fill('x') })).toBe(true);
    expect(ok({ ...valid, documents: [{ text: 'a' }] })).toBe(false);
  });

  it('refuses an unknown field and a bad top_n', () => {
    expect(ok({ ...valid, extra: 1 })).toBe(false);
    for (const top_n of [0, -1, 1.5, '2']) expect(ok({ ...valid, top_n })).toBe(false);
  });
});

describe('POST /v1/rerank response', () => {
  const response = {
    id: 'gen_1',
    model: 'cohere/rerank-v3@2024-01',
    results: [
      { index: 1, relevance_score: 0.9, document: { text: 'b' } },
      { index: 0, relevance_score: 0.1 },
    ],
    usage: { prompt_tokens: 10, total_tokens: 10 },
  };
  const parses = (value: unknown) => rerankResponseSchema.safeParse(value).success;

  it('parses, and refuses duplicate indexes, unknown members and an amount', () => {
    expect(parses(response)).toBe(true);
    expect(parses({ ...response, results: [response.results[0], response.results[0]] })).toBe(
      false,
    );
    expect(parses({ ...response, cost: { amount: '1' } })).toBe(false);
  });
});

describe('the signed envelope of a rerank request', () => {
  const envelope = {
    schemaVersion: 2 as const,
    attribution: {
      principal: {
        billing: { accountId: 'acc_1' },
        applicationId: 'app_1',
        credentialId: 'cred_1',
        environment: 'production' as const,
        inferenceScopes: ['inference:invoke'],
      },
      requestId: 'req_1',
    },
    target: { kind: 'model' as const, modelReference: 'cohere/rerank-v3.5' },
    modality: 'text' as const,
    input: { format: 'text' as const, text: 'which is about foxes' },
    stream: false,
    sampling: {},
    tools: [],
    rerank: { documents: ['a fox', 'a dog'], topN: 1, returnDocuments: true },
    client: {
      apiFormat: 'rerank' as const,
      endpoint: '/v1/rerank',
      receivedAt: '2026-08-15T09:41:00.000Z',
    },
    routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
  };
  const accepts = (value: unknown) => inferenceRequestSchema.safeParse(value).success;

  it('needs its documents, and only a rerank request has them', () => {
    expect(accepts(envelope)).toBe(true);
    const { rerank: _rerank, ...withoutMember } = envelope;
    expect(accepts(withoutMember)).toBe(false);
    expect(accepts({ ...envelope, client: { ...envelope.client, apiFormat: 'embeddings' } })).toBe(
      false,
    );
    expect(accepts({ ...envelope, stream: true })).toBe(false);
    expect(accepts({ ...envelope, input: { format: 'text', text: '' } })).toBe(false);
    expect(accepts({ ...envelope, rerank: { documents: [] } })).toBe(false);
    expect(accepts({ ...envelope, rerank: { documents: [''] } })).toBe(false);
    expect(accepts({ ...envelope, rerank: { documents: ['a'], topN: 0 } })).toBe(false);
    expect(accepts({ ...envelope, rerank: { documents: ['a'], extra: 1 } })).toBe(false);
  });
});
