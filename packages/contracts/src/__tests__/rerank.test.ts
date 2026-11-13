import { describe, expect, it } from 'bun:test';
import { MAX_RERANK_DOCUMENTS, rerankRequestSchema, rerankResponseSchema } from '../index.js';
import { grantOperationSchema } from '../internal.js';

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
