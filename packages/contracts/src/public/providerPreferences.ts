/**
 * The `provider` object a caller may send on `POST /v1/chat/completions` and
 * `POST /v1/responses` (Kaana ADR 0008, decision 6).
 *
 * It is a preference that can only NARROW. Oxy signs what a request may use and
 * Kaana resolves the grant into routes; this object filters or reorders what
 * the grant already admitted. It cannot add a provider, region, funding tier or
 * price the grant did not, and a request it leaves with no route is refused
 * rather than widened.
 *
 * The names are OpenRouter's, so a client written for OpenRouter's `provider`
 * object works, and only the controls Kaana can honour are accepted: `zdr`,
 * `data_collection`, `preferred_*` with a percentile object and
 * `enforce_distillable_text` are refused by `.strict()` rather than silently
 * ignored, because the data policy is Oxy's and is applied when it computes the
 * grant.
 *
 * `max_price` and `sort: "price"` read the DECLARED LIST PRICE of a model line,
 * the price Oxy rates the grant at, in USD per million tokens. They are about
 * which model line serves the request, because every route of one line costs
 * the customer the same: they never expose an amount, and a line whose price
 * the catalogue does not state is neither within a ceiling nor cheap.
 *
 * Provider names here are Kaana provider slugs as a caller sees them in the
 * catalogue. A response never names one.
 */

import { z } from 'zod';
import { maxUtf16Length, minUtf16Length } from '../stringBounds.js';

const providerSlugSchema = z
  .string()
  .check(minUtf16Length(1))
  .check(maxUtf16Length(64))
  .regex(/^[a-z0-9][a-z0-9._-]*$/);

const providerListSchema = z.array(providerSlugSchema).min(1).max(16);

/** Quantization levels a route can be published at (the OpenRouter vocabulary). */
export const providerQuantizationSchema = z.enum([
  'int4',
  'int8',
  'fp4',
  'mxfp4',
  'nvfp4',
  'fp6',
  'fp8',
  'mxfp8',
  'fp16',
  'bf16',
  'fp32',
  'unknown',
]);

export const providerPreferencesSchema = z
  .object({
    /** Providers to try first, in this order. Others still follow unless `allow_fallbacks` is false. */
    order: providerListSchema.optional(),
    /** Allow-list: only these providers may serve the request. */
    only: providerListSchema.optional(),
    /** Skip-list: these providers never serve the request. */
    ignore: providerListSchema.optional(),
    /**
     * False keeps a single provider: the first of `order` that is admitted, or
     * the best-ranked one. The request is never moved to another.
     */
    allow_fallbacks: z.boolean().optional(),
    /** True keeps only routes that accept every parameter the request carries. */
    require_parameters: z.boolean().optional(),
    /** Keep only routes published at one of these quantizations; `unknown` admits unstated ones. */
    quantizations: z.array(providerQuantizationSchema).min(1).max(12).optional(),
    /**
     * Prefer the model line with the lowest declared list price, or the route
     * with the lowest recent latency or the highest recent throughput.
     */
    sort: z.enum(['price', 'latency', 'throughput']).optional(),
    /**
     * A ceiling on the declared list price of the model line, in USD per million
     * tokens. A line over it, or one whose price is not stated, is not used; the
     * request is refused rather than served above it.
     */
    max_price: z
      .object({
        prompt: z.number().positive().max(1_000_000).optional(),
        completion: z.number().positive().max(1_000_000).optional(),
      })
      .strict()
      .refine((ceiling) => ceiling.prompt !== undefined || ceiling.completion !== undefined, {
        message: 'max_price names prompt, completion or both',
      })
      .optional(),
    /** Seconds. A route slower than this recently is tried later, never excluded (p50). */
    preferred_max_latency: z.number().positive().max(600).optional(),
    /** Output tokens per second. A route slower than this recently is tried later, never excluded (p50). */
    preferred_min_throughput: z.number().positive().max(100_000).optional(),
  })
  .strict();

export type ProviderPreferences = z.infer<typeof providerPreferencesSchema>;

/**
 * An opaque caller-chosen string that groups requests sharing a prompt prefix.
 * Kaana keeps such requests on the deployment that last served them while it is
 * still admitted, so the provider's prompt cache stays warm. It is never stored
 * with a prompt and never leaves Kaana.
 */
export const promptCacheKeySchema = z.string().check(minUtf16Length(1)).check(maxUtf16Length(256));
