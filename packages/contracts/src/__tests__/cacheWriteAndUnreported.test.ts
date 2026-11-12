/**
 * Contract set 8.4.0: the usage unit `cache_write_input_tokens` and the optional
 * `capabilities.unreported`. Both are declared before anything emits them.
 */
import { describe, expect, it } from 'bun:test';
import {
  INFERENCE_CONTRACT_VERSION,
  modelCapabilitiesSchema,
  UNREPORTED_CAPABILITY_KEYS,
  USAGE_UNITS,
  usageQuantitySchema,
  usageUnitSchema,
} from '../index.js';

describe('cache_write_input_tokens', () => {
  it('is the last usage unit, so the list stays append-only', () => {
    expect(USAGE_UNITS.at(-1)).toBe('cache_write_input_tokens');
    expect(USAGE_UNITS.indexOf('input_transcription_milliseconds')).toBe(USAGE_UNITS.length - 2);
    expect(usageUnitSchema.safeParse('cache_write_input_tokens').success).toBe(true);
  });

  it('is accepted in a usage quantity, and only as a whole non-negative number', () => {
    expect(
      usageQuantitySchema.safeParse({ unit: 'cache_write_input_tokens', quantity: 1200 }).success,
    ).toBe(true);
    expect(
      usageQuantitySchema.safeParse({ unit: 'cache_write_input_tokens', quantity: -1 }).success,
    ).toBe(false);
    expect(
      usageQuantitySchema.safeParse({ unit: 'cache_write_input_tokens', quantity: 1.5 }).success,
    ).toBe(false);
  });

  it('states contract set 8.4.0', () => {
    expect(INFERENCE_CONTRACT_VERSION).toBe('8.4.0');
  });
});

const capabilities = {
  inputModalities: ['text'],
  outputModalities: ['text'],
  tools: false,
  parallelToolCalls: false,
  structuredOutput: false,
  jsonMode: false,
  reasoning: false,
  streaming: true,
  promptCaching: false,
  maxContextTokens: 8000,
  maxOutputTokens: 1000,
};

describe('capabilities.unreported', () => {
  it('is optional: a catalogue without it parses and stays without it', () => {
    const parsed = modelCapabilitiesSchema.parse(capabilities);
    expect(parsed.unreported).toBeUndefined();
  });

  it('names exactly the five required booleans', () => {
    expect([...UNREPORTED_CAPABILITY_KEYS].sort()).toEqual([
      'jsonMode',
      'parallelToolCalls',
      'promptCaching',
      'structuredOutput',
      'tools',
    ]);
  });

  it('accepts listed keys whose boolean is false, including an empty list', () => {
    expect(
      modelCapabilitiesSchema.parse({ ...capabilities, unreported: ['jsonMode', 'promptCaching'] })
        .unreported,
    ).toEqual(['jsonMode', 'promptCaching']);
    expect(modelCapabilitiesSchema.safeParse({ ...capabilities, unreported: [] }).success).toBe(
      true,
    );
  });

  it('rejects a duplicate key', () => {
    expect(
      modelCapabilitiesSchema.safeParse({ ...capabilities, unreported: ['tools', 'tools'] })
        .success,
    ).toBe(false);
  });

  it('rejects a key that is not one of the five booleans', () => {
    expect(
      modelCapabilitiesSchema.safeParse({ ...capabilities, unreported: ['streaming'] }).success,
    ).toBe(false);
    expect(
      modelCapabilitiesSchema.safeParse({ ...capabilities, unreported: ['vision'] }).success,
    ).toBe(false);
  });

  it('rejects a listed key whose boolean is true', () => {
    expect(
      modelCapabilitiesSchema.safeParse({ ...capabilities, tools: true, unreported: ['tools'] })
        .success,
    ).toBe(false);
  });
});
