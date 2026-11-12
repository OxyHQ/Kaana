/**
 * `POST /v1/messages` — the Anthropic-Messages-compatible surface.
 *
 * It speaks the flat Anthropic request, response and stream shapes so an
 * unmodified Anthropic SDK or Claude Code works with
 * `ANTHROPIC_BASE_URL=https://kaana.ai` and an `oxy_sk_…` key (sent as
 * `x-api-key` or as `Authorization: Bearer`). Like `/v1/chat/completions` it
 * gains NO Kaana-specific request field, and it is `.strict()` everywhere: a
 * stock SDK sending a parameter Kaana does not implement is told so rather than
 * having it silently dropped.
 *
 * ## What is refused, and why
 *
 *  - **`cache_control`, anywhere** (on a block, a tool, `system` or the
 *    request). Kaana's usage units have no unit for a cache WRITE (see the
 *    Anthropic adapter's usage meter): a cache write is billed at a premium
 *    over input, so honouring the marker while folding the write into
 *    `input_tokens` would report less than the request cost. The marker is
 *    refused, by name and path, instead of being dropped.
 *  - **`thinking.display`** and every other `thinking` member this file does
 *    not name.
 *  - **Server tools** (`web_search`, `bash`, `text_editor`, …), `mcp_servers`,
 *    `container`, `service_tier`, `disable_parallel_tool_use`, `document`
 *    blocks and any other member this file does not name.
 *  - **An image `url` source**: nothing fetches an image URL (ADR 0007); a
 *    `base64` source is carried inline under the image rules.
 *
 * ## Reasoning (contract set 8.6.0, ADR 0009 decision 2)
 *
 * `thinking: {type: 'enabled', budget_tokens}` is the contract's
 * `reasoning.maxTokens` and `thinking: {type: 'adaptive'}` opts in to reasoning
 * with the route's own sizing; `output_config.effort` is `reasoning.effort`, and
 * a budget beside an effort is refused (the contract treats them as
 * alternatives). A route that cannot take a budget refuses it by name.
 *
 * An answer carries `thinking` blocks before its text and tool calls, and an
 * assistant message sent back carries them again, UNMODIFIED, ahead of its
 * `tool_use` blocks. The block's `signature` is the contract's opaque
 * continuation token (the sealed string a `reasoning` part carries), which is
 * why a stock Anthropic SDK continues a tool turn without knowing anything
 * else: it returns what it received. `redacted_thinking` is accepted on the way
 * in (its `data` is the same token) and never produced.
 *
 * `stop_reason: 'stop_sequence'` is never produced: the finish reason Kaana
 * measures does not say WHICH stop sequence ended the turn, and `stop_sequence`
 * would then have to name one. A turn that ended on a stop sequence reports
 * `end_turn`.
 *
 * `metadata.user_id` is accepted and neither forwarded nor stored: it is an
 * end-user identifier, and Kaana persists none.
 *
 * `usage` is derived from the measured units: `input_tokens`, `output_tokens`
 * (reasoning included, as Anthropic counts it) and, only when the route
 * measured it, `cache_read_input_tokens`. There is no `cache_creation_*`.
 */

import { z } from 'zod';
import { maxUtf16Length, minUtf16Length } from '../stringBounds.js';
import { reasoningEffortSchema } from './catalogue.js';
import { modelTargetSchema } from './responses.js';

const nonEmpty = (max: number) => z.string().check(minUtf16Length(1)).check(maxUtf16Length(max));

const textBlockSchema = z.object({ type: z.literal('text'), text: z.string() }).strict();

const imageBlockSchema = z
  .object({
    type: z.literal('image'),
    source: z.discriminatedUnion('type', [
      z
        .object({
          type: z.literal('base64'),
          media_type: nonEmpty(255),
          data: z.string().check(minUtf16Length(1)),
        })
        .strict(),
      /** Parses so the refusal can name it; Kaana fetches no image URL. */
      z.object({ type: z.literal('url'), url: nonEmpty(4096) }).strict(),
    ]),
  })
  .strict();

/**
 * An assistant's reasoning. `thinking` is the readable text (empty when the
 * model returned none); `signature` is the continuation token to return
 * unmodified, empty only for a block that cannot be continued.
 */
const thinkingBlockSchema = z
  .object({
    type: z.literal('thinking'),
    thinking: z.string(),
    signature: z.string(),
  })
  .strict();

const redactedThinkingBlockSchema = z
  .object({ type: z.literal('redacted_thinking'), data: nonEmpty(1_000_000) })
  .strict();

const toolUseBlockSchema = z
  .object({
    type: z.literal('tool_use'),
    id: nonEmpty(128),
    name: nonEmpty(128),
    input: z.record(z.string(), z.unknown()),
  })
  .strict();

const toolResultBlockSchema = z
  .object({
    type: z.literal('tool_result'),
    tool_use_id: nonEmpty(128),
    content: z
      .union([z.string(), z.array(z.union([textBlockSchema, imageBlockSchema]))])
      .optional(),
    /**
     * Accepted. The contract's tool message has no error flag, so the result's
     * own content is what the model reads; the flag is not forwarded.
     */
    is_error: z.boolean().optional(),
  })
  .strict();

const messageSchema = z
  .object({
    role: z.enum(['user', 'assistant']),
    content: z.union([
      z.string(),
      z
        .array(
          z.union([
            textBlockSchema,
            imageBlockSchema,
            toolUseBlockSchema,
            toolResultBlockSchema,
            thinkingBlockSchema,
            redactedThinkingBlockSchema,
          ]),
        )
        .min(1),
    ]),
  })
  .strict()
  .superRefine((message, ctx) => {
    if (typeof message.content === 'string') return;
    for (const [index, block] of message.content.entries()) {
      const reasoning = block.type === 'thinking' || block.type === 'redacted_thinking';
      const allowed =
        message.role === 'assistant'
          ? block.type === 'text' || block.type === 'tool_use' || reasoning
          : block.type !== 'tool_use' && !reasoning;
      if (!allowed) {
        ctx.addIssue({
          code: 'custom',
          path: ['content', index, 'type'],
          message: `a ${message.role} message does not carry a ${block.type} block`,
        });
      }
    }
  });

const toolSchema = z
  .object({
    type: z.literal('custom').optional(),
    name: nonEmpty(128),
    description: z.string().check(maxUtf16Length(32768)).optional(),
    input_schema: z.record(z.string(), z.unknown()),
    strict: z.boolean().optional(),
  })
  .strict();

const toolChoiceSchema = z.discriminatedUnion('type', [
  z.object({ type: z.literal('auto') }).strict(),
  z.object({ type: z.literal('any') }).strict(),
  z.object({ type: z.literal('none') }).strict(),
  z.object({ type: z.literal('tool'), name: nonEmpty(128) }).strict(),
]);

export const anthropicMessagesRequestSchema = z
  .object({
    /** A model reference (`publisher/model[@revision]`) or a power level. */
    model: modelTargetSchema,
    /** Required, as Anthropic requires it. */
    max_tokens: z.number().int().positive().safe(),
    messages: z.array(messageSchema).min(1),
    system: z.union([z.string(), z.array(textBlockSchema)]).optional(),
    temperature: z.number().min(0).max(1).optional(),
    top_p: z.number().min(0).max(1).optional(),
    top_k: z.number().int().positive().safe().optional(),
    stop_sequences: z.array(nonEmpty(256)).max(8).optional(),
    stream: z.boolean().optional(),
    tools: z.array(toolSchema).max(128).optional(),
    tool_choice: toolChoiceSchema.optional(),
    /**
     * `disabled` asks for nothing; `adaptive` opts in to reasoning (the
     * contract's `reasoning: {}`); `enabled` sets a token budget
     * (`reasoning.maxTokens`). See the file comment.
     */
    thinking: z
      .discriminatedUnion('type', [
        z.object({ type: z.literal('disabled') }).strict(),
        z.object({ type: z.literal('adaptive') }).strict(),
        z
          .object({
            type: z.literal('enabled'),
            budget_tokens: z.number().int().positive().safe(),
          })
          .strict(),
      ])
      .optional(),
    /** `effort` is the contract's reasoning effort; no other member is carried. */
    output_config: z.object({ effort: reasoningEffortSchema.optional() }).strict().optional(),
    /** Accepted and not forwarded or stored. */
    metadata: z
      .object({ user_id: z.string().check(maxUtf16Length(512)).nullable().optional() })
      .strict()
      .optional(),
  })
  .strict()
  .superRefine((request, ctx) => {
    if (request.tool_choice !== undefined && (request.tools ?? []).length === 0) {
      ctx.addIssue({
        code: 'custom',
        path: ['tool_choice'],
        message: 'a tool choice requires at least one tool definition',
      });
    }
  });

const usageSchema = z
  .object({
    input_tokens: z.number().int().nonnegative(),
    output_tokens: z.number().int().nonnegative(),
    /** Present only when the route measured a cache read. */
    cache_read_input_tokens: z.number().int().nonnegative().optional(),
  })
  .strict();

const stopReasonSchema = z.enum(['end_turn', 'max_tokens', 'tool_use', 'refusal']);

const outputBlockSchema = z.discriminatedUnion('type', [
  thinkingBlockSchema,
  textBlockSchema,
  toolUseBlockSchema.extend({ input: z.record(z.string(), z.unknown()) }),
]);

const messageBodySchema = z.object({
  id: nonEmpty(256),
  type: z.literal('message'),
  role: z.literal('assistant'),
  /** The revision that served the request. */
  model: nonEmpty(256),
});

/** The non-streaming body. The request id rides in `Kaana-Request-Id` and `Request-Id`. */
export const anthropicMessageSchema = messageBodySchema
  .extend({
    content: z.array(outputBlockSchema),
    stop_reason: stopReasonSchema,
    stop_sequence: z.literal(null),
    usage: usageSchema,
  })
  .strict();

/** The error body, and the data of a stream's `event: error` frame. */
export const anthropicErrorSchema = z
  .object({
    type: z.literal('error'),
    error: z
      .object({
        type: z.enum([
          'invalid_request_error',
          'authentication_error',
          'billing_error',
          'permission_error',
          'not_found_error',
          'request_too_large',
          'rate_limit_error',
          'timeout_error',
          'overloaded_error',
          'api_error',
        ]),
        message: nonEmpty(4096),
      })
      .strict(),
    request_id: nonEmpty(256).optional(),
  })
  .strict();

/**
 * One frame of a streamed message: `event: <type>` and this as its data. The
 * input tokens are known only when the route reports usage, so `message_start`
 * carries zeros and the final `message_delta` the measured totals. A stream that
 * fails after its first frame ends with an `error` frame and no `message_stop`.
 */
export const anthropicStreamEventSchema = z.discriminatedUnion('type', [
  z
    .object({
      type: z.literal('message_start'),
      message: messageBodySchema
        .extend({
          content: z.array(outputBlockSchema).length(0),
          stop_reason: z.literal(null),
          stop_sequence: z.literal(null),
          usage: usageSchema,
        })
        .strict(),
    })
    .strict(),
  z
    .object({
      type: z.literal('content_block_start'),
      index: z.number().int().nonnegative(),
      content_block: z.discriminatedUnion('type', [
        z
          .object({
            type: z.literal('thinking'),
            thinking: z.literal(''),
            signature: z.literal(''),
          })
          .strict(),
        z.object({ type: z.literal('text'), text: z.literal('') }).strict(),
        toolUseBlockSchema.extend({ input: z.object({}).strict() }),
      ]),
    })
    .strict(),
  z
    .object({
      type: z.literal('content_block_delta'),
      index: z.number().int().nonnegative(),
      delta: z.discriminatedUnion('type', [
        z.object({ type: z.literal('text_delta'), text: z.string() }).strict(),
        z.object({ type: z.literal('thinking_delta'), thinking: z.string() }).strict(),
        z.object({ type: z.literal('signature_delta'), signature: z.string() }).strict(),
        z.object({ type: z.literal('input_json_delta'), partial_json: z.string() }).strict(),
      ]),
    })
    .strict(),
  z
    .object({ type: z.literal('content_block_stop'), index: z.number().int().nonnegative() })
    .strict(),
  z
    .object({
      type: z.literal('message_delta'),
      delta: z.object({ stop_reason: stopReasonSchema, stop_sequence: z.literal(null) }).strict(),
      usage: usageSchema,
    })
    .strict(),
  z.object({ type: z.literal('message_stop') }).strict(),
  z.object({ type: z.literal('ping') }).strict(),
  anthropicErrorSchema,
]);

export type AnthropicMessagesRequest = z.infer<typeof anthropicMessagesRequestSchema>;
export type AnthropicMessage = z.infer<typeof anthropicMessageSchema>;
export type AnthropicError = z.infer<typeof anthropicErrorSchema>;
export type AnthropicStreamEvent = z.infer<typeof anthropicStreamEventSchema>;
