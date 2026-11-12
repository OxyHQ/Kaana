/**
 * Contract set 8.8.0: the optional prompt-cache breakpoint on a content part
 * and on a tool definition.
 */
import { describe, expect, it } from 'bun:test';
import {
  INFERENCE_CONTRACT_VERSION,
  inferenceContentPartSchema,
  inferenceMessageSchema,
  realtimeToolDefinitionSchema,
  toolDefinitionSchema,
} from '../index.js';

const inline = { kind: 'inline', mediaType: 'image/png', data: 'AAAA' } as const;

describe('cacheBreakpoint', () => {
  it('is carried by contract set 8.8.0 or any later one', () => {
    const [major = 0, minor = 0] = INFERENCE_CONTRACT_VERSION.split('.').map(Number);
    expect(major > 8 || (major === 8 && minor >= 8)).toBe(true);
  });

  it.each([
    ['text', { type: 'text', text: 'rules' }],
    ['image', { type: 'image', source: inline }],
    ['file', { type: 'file', source: inline, filename: 'a.pdf' }],
  ])('is accepted on a %s part, as a boolean only', (_name, part) => {
    expect(inferenceContentPartSchema.safeParse({ ...part, cacheBreakpoint: true }).success).toBe(
      true,
    );
    expect(inferenceContentPartSchema.safeParse({ ...part, cacheBreakpoint: false }).success).toBe(
      true,
    );
    // The marker is a hint with no lifetime to state: a ttl, or any value but a
    // boolean, is not it.
    expect(inferenceContentPartSchema.safeParse({ ...part, cacheBreakpoint: 'yes' }).success).toBe(
      false,
    );
    expect(
      inferenceContentPartSchema.safeParse({ ...part, cacheBreakpoint: { ttl: '1h' } }).success,
    ).toBe(false);
    // The control: without it the same part parses and carries none.
    const plain = inferenceContentPartSchema.parse(part);
    expect('cacheBreakpoint' in plain).toBe(false);
  });

  it.each([
    ['audio', { type: 'audio', source: inline }],
    ['refusal', { type: 'refusal', text: 'no' }],
    ['reasoning', { type: 'reasoning', text: 'hm' }],
  ])('is refused on a %s part, which no provider caches', (_name, part) => {
    expect(inferenceContentPartSchema.safeParse(part).success).toBe(true);
    expect(inferenceContentPartSchema.safeParse({ ...part, cacheBreakpoint: true }).success).toBe(
      false,
    );
  });

  it('survives a parse of a system message, in the part that carried it', () => {
    const message = inferenceMessageSchema.parse({
      role: 'system',
      content: [
        { type: 'text', text: 'a' },
        { type: 'text', text: 'b', cacheBreakpoint: true },
      ],
    });
    expect(message.content.map((part) => 'cacheBreakpoint' in part)).toEqual([false, true]);
  });

  it('is accepted on a tool definition but not on a realtime one', () => {
    const tool = { type: 'function', name: 'lookup', parameters: { type: 'object' } };
    expect(toolDefinitionSchema.safeParse({ ...tool, cacheBreakpoint: true }).success).toBe(true);
    expect(toolDefinitionSchema.safeParse({ ...tool, cacheBreakpoint: 1 }).success).toBe(false);
    expect(realtimeToolDefinitionSchema.safeParse(tool).success).toBe(true);
    expect(realtimeToolDefinitionSchema.safeParse({ ...tool, cacheBreakpoint: true }).success).toBe(
      false,
    );
  });
});
