import { describe, expect, it } from 'bun:test';
import { z } from 'zod';
import { requestIdSchema } from '../index.js';

/**
 * The contract bounds strings in UTF-16 code units: Go counts them that way
 * (`utf16Length` in `internal/contract`), the adapters count characters that
 * way, and the frozen cross-language fixtures assume it. Zod 4.5 began counting
 * Unicode code points instead, which would let a 129-emoji identity (258 units)
 * pass a 256-unit bound here and fail in Go. The dependency is therefore held
 * below 4.5; this suite is what fails if it is bumped past it.
 */
describe('string bounds are UTF-16 code units', () => {
  it('counts an astral character as two units', () => {
    expect(z.string().max(3).safeParse('😀😀').success).toBe(false);
    expect(z.string().max(4).safeParse('😀😀').success).toBe(true);
    expect(z.string().min(2).safeParse('😀').success).toBe(true);
  });

  it('holds a contract identity to its unit bound', () => {
    expect(requestIdSchema.safeParse('😀'.repeat(64)).success).toBe(true);
    expect(requestIdSchema.safeParse('😀'.repeat(65)).success).toBe(false);
  });
});
