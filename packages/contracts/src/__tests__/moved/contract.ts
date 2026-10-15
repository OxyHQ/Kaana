/**
 * The whole contract, both surfaces, for the suites moved from
 * `@oxy.so/contracts` 4.12.0 (`packages/contracts/src/__tests__/inference.*`).
 * They were written against one barrel; this keeps them byte-for-byte about the
 * same assertions while the shapes live in two entry points.
 */
import type { z } from 'zod';

export * from '../../index.js';
export * from '../../internal.js';

/** The `@oxy.so/contracts` helper the moved suites use: the parsed value, or `null`. */
export function safeParseContract<T>(schema: z.ZodType<T>, data: unknown): T | null {
  const result = schema.safeParse(data);
  return result.success ? result.data : null;
}
