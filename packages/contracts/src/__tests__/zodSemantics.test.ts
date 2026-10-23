import { describe, expect, it } from 'bun:test';
import { z } from 'zod';
import { inferenceTimestampSchema, requestIdSchema } from '../index.js';
import { maxUtf16Length, minUtf16Length, utf16Length } from '../stringBounds.js';

/**
 * The contract bounds strings in UTF-16 code units: Go counts them that way
 * (`utf16Length` in `internal/contract`), the adapters count characters that
 * way, and the frozen cross-language fixtures assume it. Zod 4.5 began counting
 * Unicode code points instead, which would let a 129-emoji identity (258 units)
 * pass a 256-unit bound here and fail in Go. The explicit contract checks keep
 * this measure while allowing patched Zod releases.
 */
describe('string bounds are UTF-16 code units', () => {
  it('counts an astral character as two units', () => {
    expect(z.string().check(maxUtf16Length(3)).safeParse('😀😀').success).toBe(false);
    expect(z.string().check(maxUtf16Length(4)).safeParse('😀😀').success).toBe(true);
    expect(z.string().check(minUtf16Length(2)).safeParse('😀').success).toBe(true);
    expect(z.string().check(utf16Length(2)).safeParse('😀').success).toBe(true);
    expect(z.string().check(utf16Length(1)).safeParse('😀').success).toBe(false);
    expect(z.string().check(utf16Length(3)).safeParse('😀').success).toBe(false);
  });

  it('keeps bounds through composition and JSON Schema generation', () => {
    const text = z.string().trim().check(minUtf16Length(2), maxUtf16Length(4));
    expect(text.optional().safeParse(undefined).success).toBe(true);
    expect(text.safeParse(' 😀😀 ').success).toBe(true);
    expect(text.safeParse(' 😀😀x ').success).toBe(false);
    expect(text.safeParse('x').success).toBe(false);
    expect(text.safeParse(123).success).toBe(false);
    expect(z.toJSONSchema(text)).toMatchObject({ type: 'string', minLength: 2, maxLength: 4 });
    expect(z.toJSONSchema(z.string().check(utf16Length(2)))).toMatchObject({
      minLength: 2,
      maxLength: 2,
    });
  });

  it('does not change Zod checks outside the contract', () => {
    expect(z.string().max(3).safeParse('😀😀').success).toBe(true);
    expect(z.string().min(2).safeParse('😀').success).toBe(false);
    expect(z.array(z.string()).max(1).safeParse(['😀', '😀']).success).toBe(false);
  });

  it('holds a contract identity to its unit bound', () => {
    expect(requestIdSchema.safeParse('😀'.repeat(64)).success).toBe(true);
    expect(requestIdSchema.safeParse('😀'.repeat(65)).success).toBe(false);
  });

  it('rejects oversized arrays before processing their elements', () => {
    let visited = 0;
    const schema = z
      .array(
        z.string().transform((value) => {
          visited++;
          return value;
        }),
      )
      .max(1);
    expect(schema.safeParse(['ok']).success).toBe(true);
    expect(visited).toBe(1);
    visited = 0;
    expect(schema.safeParse(Array.from({ length: 10_000 }, () => 'excess')).success).toBe(false);
    expect(visited).toBe(0);
  });

  it('preserves UTC timestamps at minute, second and fractional precision', () => {
    for (const value of ['2024-02-29T12:34Z', '2024-02-29T12:34:56Z', '2024-02-29T12:34:56.123Z']) {
      expect(inferenceTimestampSchema.safeParse(value).success).toBe(true);
    }
    for (const value of [
      '2023-02-29T12:34Z',
      '2024-02-29T24:00Z',
      '2024-02-29T12:34',
      '2024-02-29T12:34:56+02:00',
    ]) {
      expect(inferenceTimestampSchema.safeParse(value).success).toBe(false);
    }
  });
});
