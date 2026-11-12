/**
 * `POST /v1/responses` — Kaana's own dialect, and the preferred endpoint.
 *
 * Moved from the Oxy edge (`packages/api/src/schemas/inferenceEdge.schemas.ts`,
 * `responsesRequestSchema` and `responsesResponseSchema`) by Kaana ADR 0002.
 * Two changes, both from the ADR: `model` names a model reference or a power
 * level and nothing else (Oxy's `routingProfile`/`routingProfileId` were Oxy
 * catalogue identities), and the response names no serving provider.
 */

import { z } from 'zod';
import { maxUtf16Length, minUtf16Length } from '../stringBounds.js';
import { powerLevelSchema } from './catalogue.js';
import { generationIdSchema, modelReferenceSchema, requestIdSchema } from './identifiers.js';
import {
  inferenceMessageSchema,
  inferenceReasoningSchema,
  requestLabelsSchema,
  toolChoiceSchema,
  toolDefinitionSchema,
} from './messages.js';
import { usageQuantitySchema } from './money.js';
import { routingPolicyReferenceSchema } from './policy.js';
import { promptCacheKeySchema, providerPreferencesSchema } from './providerPreferences.js';
import { inferenceFinishReasonSchema } from './streamEvents.js';

/**
 * What a `model` field may name: a concrete model reference
 * (`publisher/model[@revision]`, served or refused, never substituted) or a
 * power level (`auto`, `instant`, … — no `/`, so the two are decidable from
 * the string alone).
 */
export const modelTargetSchema = z.union([modelReferenceSchema, powerLevelSchema]);

/**
 * The response format of `POST /v1/responses`: the signed request's format
 * (`responseFormatSchema`) and, on the two JSON formats, `repair` (contract set
 * 8.9.0, Kaana ADR 0009 decision 5).
 *
 * `repair: true` asks Kaana to repair nearly valid JSON in the final text of a
 * non-streaming answer that ended with `stop`: a surrounding code fence, prose
 * around the value, trailing commas, closers a normal stop forgot. It never
 * changes text that already parses, never repairs an answer cut by the token
 * limit (`length`) or ended by a filter, a refusal or a tool call, and checks
 * nothing against `schema` (Kaana carries no validator). A repaired answer is
 * marked by the response header `Kaana-Repaired: true`, never by a body
 * member. It is refused on `text` and with `stream: true`, and it is not part of
 * the signed request: no provider and no Oxy sees it. Default off.
 */
export const responsesResponseFormatSchema = z.discriminatedUnion('type', [
  z.object({ type: z.literal('text') }).strict(),
  z.object({ type: z.literal('json_object'), repair: z.boolean().optional() }).strict(),
  z
    .object({
      type: z.literal('json_schema'),
      name: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)),
      schema: z.record(z.string(), z.unknown()),
      strict: z.boolean(),
      repair: z.boolean().optional(),
    })
    .strict(),
]);

/** The message-or-text input `/v1/responses` accepts. */
const responsesInputSchema = z.union([
  z.string().check(minUtf16Length(1)),
  z.array(inferenceMessageSchema).min(1),
]);

/**
 * The request. `model` may be absent: the application's routing policy in Oxy
 * Console may name a default target, and the request is refused only when it
 * names none either.
 *
 * `.strict()`: a field Kaana does not implement is refused rather than
 * silently ignored, so nobody is billed for a request that dropped half of
 * what it asked for.
 */
export const responsesRequestSchema = z
  .object({
    model: modelTargetSchema.optional(),
    input: responsesInputSchema,
    maxOutputTokens: z.number().int().positive().safe().optional(),
    stream: z.boolean().optional(),
    temperature: z.number().min(0).max(2).optional(),
    topP: z.number().min(0).max(1).optional(),
    topK: z.number().int().positive().safe().optional(),
    frequencyPenalty: z.number().min(-2).max(2).optional(),
    presencePenalty: z.number().min(-2).max(2).optional(),
    seed: z.number().int().safe().optional(),
    stopSequences: z
      .array(z.string().check(minUtf16Length(1)).check(maxUtf16Length(256)))
      .max(8)
      .optional(),
    tools: z.array(toolDefinitionSchema).max(128).optional(),
    toolChoice: toolChoiceSchema.optional(),
    responseFormat: responsesResponseFormatSchema.optional(),
    /** Refused when the resolved model does not advertise the effort. */
    reasoning: inferenceReasoningSchema.optional(),
    labels: requestLabelsSchema.optional(),
    /** The caller's own correlation id, echoed on the response. */
    clientRequestId: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)).optional(),
    /** Routing preferences that can only narrow what the grant admits (ADR 0008). */
    provider: providerPreferencesSchema.optional(),
    /** Keeps requests sharing a prompt prefix on one deployment while its cache is warm. */
    promptCacheKey: promptCacheKeySchema.optional(),
  })
  .strict()
  .superRefine((request, ctx) => {
    if (
      request.responseFormat !== undefined &&
      'repair' in request.responseFormat &&
      request.responseFormat.repair === true &&
      request.stream === true
    ) {
      ctx.addIssue({
        code: 'custom',
        path: ['responseFormat', 'repair'],
        message: 'repair works on a whole answer and cannot be used with stream: true',
      });
    }
    if (request.toolChoice !== undefined && (request.tools ?? []).length === 0) {
      ctx.addIssue({
        code: 'custom',
        path: ['toolChoice'],
        message: 'a tool choice requires at least one tool definition',
      });
    }
    const names = (request.tools ?? []).map((tool) => tool.name);
    if (new Set(names).size !== names.length) {
      ctx.addIssue({
        code: 'custom',
        path: ['tools'],
        message: 'tool names must be unique within one request',
      });
    }
  });

/**
 * The non-streaming body. A streamed request answers with
 * `responseStreamEventSchema` frames instead.
 */
export const responseSchema = z
  .object({
    schemaVersion: z.literal(1),
    requestId: requestIdSchema,
    /** Absent when no generation id was reported for the request. */
    generationId: generationIdSchema.optional(),
    /** The revision that actually served the request, which may differ from the ask. */
    model: modelReferenceSchema,
    finishReason: inferenceFinishReasonSchema,
    output: z.array(inferenceMessageSchema),
    usage: z.array(usageQuantitySchema),
    /** The customer's own routing-policy revision that served it. */
    routingPolicy: routingPolicyReferenceSchema,
    /** Kaana's own measurement of the request, in whole milliseconds. */
    latencyMs: z.number().int().nonnegative(),
  })
  .strict();

export type ResponsesResponseFormat = z.infer<typeof responsesResponseFormatSchema>;
export type ModelTarget = z.infer<typeof modelTargetSchema>;
export type ResponsesRequest = z.infer<typeof responsesRequestSchema>;
export type Response = z.infer<typeof responseSchema>;
