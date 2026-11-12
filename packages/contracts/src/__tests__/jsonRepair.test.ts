import { describe, expect, it } from 'bun:test';
import {
  INFERENCE_CONTRACT_VERSION,
  KAANA_REPAIRED_HEADER,
  responseFormatSchema,
  responsesRequestSchema,
} from '../index.js';

const model = 'openai/gpt-5';
const schemaFormat = {
  type: 'json_schema',
  name: 'answer',
  schema: { type: 'object' },
  strict: true,
} as const;

describe('responseFormat.repair (contract set 8.9.0, ADR 0009 decision 5)', () => {
  it('is an optional boolean on the two JSON formats of /v1/responses', () => {
    for (const responseFormat of [
      { type: 'json_object', repair: true },
      { type: 'json_object', repair: false },
      { type: 'json_object' },
      { ...schemaFormat, repair: true },
      { ...schemaFormat },
    ]) {
      expect(responsesRequestSchema.safeParse({ model, input: 'x', responseFormat }).success).toBe(
        true,
      );
    }
  });

  it('is refused on a text format, as a non-boolean and with a stream', () => {
    const refused = [
      { responseFormat: { type: 'text', repair: true } },
      { responseFormat: { type: 'json_object', repair: 'yes' } },
      { responseFormat: { type: 'json_object', repair: true }, stream: true },
      { responseFormat: { ...schemaFormat, repair: true }, stream: true },
    ];
    for (const extra of refused) {
      expect(responsesRequestSchema.safeParse({ model, input: 'x', ...extra }).success).toBe(false);
    }
    // Off, with a stream, is the request it always was.
    expect(
      responsesRequestSchema.safeParse({
        model,
        input: 'x',
        stream: true,
        responseFormat: { type: 'json_object', repair: false },
      }).success,
    ).toBe(true);
  });

  it('is not part of the signed request format', () => {
    expect(responseFormatSchema.safeParse({ type: 'json_object', repair: true }).success).toBe(
      false,
    );
    expect(responseFormatSchema.safeParse({ type: 'json_object' }).success).toBe(true);
  });

  it('is marked by a header, and the contract set moved to 8.9.0', () => {
    expect(KAANA_REPAIRED_HEADER).toBe('Kaana-Repaired');
    const [major = 0, minor = 0] = INFERENCE_CONTRACT_VERSION.split('.').map(Number);
    expect(major > 8 || (major === 8 && minor >= 9)).toBe(true);
  });
});
