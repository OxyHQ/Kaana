/**
 * Messages, content parts, tools and generation controls shared by every
 * public request and by the signed envelope.
 *
 * Moved from `@oxy.so/contracts` 4.12.0 (`src/inference/request.ts`) by Kaana ADR 0002.
 */

import { z } from 'zod';
import { maxUtf16Length, minUtf16Length } from '../stringBounds.js';
import { languageTagSchema } from './audioFormats.js';
import { reasoningEffortSchema } from './catalogue.js';

/**
 * Where binary or remote content comes from.
 *
 * `url` is fetched by the data plane; `inline` carries base64 the customer sent
 * with the request. Both are transient: neither is persisted by default, and
 * neither appears in a receipt, a log line or a telemetry event.
 */
export const inferenceContentSourceSchema = z.discriminatedUnion('kind', [
  z
    .object({
      kind: z.literal('url'),
      url: z.string().check(minUtf16Length(1)).check(maxUtf16Length(4096)),
    })
    .strict(),
  z
    .object({
      kind: z.literal('inline'),
      mediaType: z.string().check(minUtf16Length(1)).check(maxUtf16Length(255)),
      data: z.string().check(minUtf16Length(1)),
    })
    .strict(),
]);

/**
 * One part of a message's content. A message is always a list of parts.
 *
 * ## `refusal` is a member and `reasoning` is not, and the asymmetry is the point
 *
 * A model that DECLINES says why, and those words are meant for the customer:
 * they are the difference between "rephrase this" and "stop asking". Before this
 * member existed there was nowhere in an {@link InferenceMessage} to put them, so
 * a non-streaming fold kept `finishReason: 'refusal'` and dropped the sentence —
 * the customer learned they were refused and not why, while a streaming caller of
 * the same request got the whole explanation on the `refusal` delta channel.
 *
 * Reasoning is the opposite case and stays absent. It is the model's private
 * working, and a `text` part is where somebody would put it — which renders
 * private reasoning to the customer AS the answer, the product bug the delta
 * channels exist to prevent. An opaque per-block reasoning blob crossing this
 * boundary needs a home nobody has chosen yet, and inventing one here would
 * choose it by accident.
 *
 * Both public dialects can carry a refusal, which is what separates the two
 * cases at the boundary as well as in principle: `refusal` is OpenAI's OWN field
 * in both of its shapes (`delta.refusal` streaming, `message.refusal`
 * non-streaming), while reasoning has no OpenAI field at all — the
 * `reasoning_content`/`reasoning` spellings an OpenAI-compatible provider emits
 * are provider extensions. So carrying the refusal costs no dialect its
 * standard-client parseability, and carrying reasoning would.
 *
 * The part is a MEMBER rather than a field because a refusal need not have text:
 * an Anthropic `stop_reason: "refusal"` maps to the finish reason and separates
 * no words from the answer, while an OpenAI-compatible `refusal` does. A required
 * field would force the first provider to invent a sentence.
 */
export const inferenceContentPartSchema = z.discriminatedUnion('type', [
  z.object({ type: z.literal('text'), text: z.string() }).strict(),
  z
    .object({
      type: z.literal('image'),
      source: inferenceContentSourceSchema,
      /** Provider-independent hint; providers that ignore it are unaffected. */
      detail: z.enum(['auto', 'low', 'high']).optional(),
    })
    .strict(),
  z.object({ type: z.literal('audio'), source: inferenceContentSourceSchema }).strict(),
  z
    .object({
      type: z.literal('file'),
      source: inferenceContentSourceSchema,
      filename: z.string().check(maxUtf16Length(255)).optional(),
    })
    .strict(),
  /**
   * A model's explanation for declining. Its own part, never a `text` one, so no
   * renderer can present a refusal as the answer that was asked for.
   */
  z.object({ type: z.literal('refusal'), text: z.string() }).strict(),
]);

/* -------------------------------------------------------------------------- */
/*  Image input (contract set 8.2.0, Kaana ADR 0007)                           */
/* -------------------------------------------------------------------------- */

/**
 * The image formats a request may carry. Every provider that serves image input
 * on a Kaana route reads these four: Anthropic (`image/jpeg`, `image/png`,
 * `image/gif`, `image/webp`) and OpenAI (PNG, JPEG, WEBP, non-animated GIF).
 */
export const INPUT_IMAGE_MEDIA_TYPES = [
  'image/png',
  'image/jpeg',
  'image/webp',
  'image/gif',
] as const;

/**
 * The most image parts one request may carry, across every message. Above 20
 * images Anthropic applies a stricter per-image dimension limit to EVERY image
 * of the request, so 20 is the most a request can carry and keep one rule per
 * image on every route.
 */
export const MAX_INPUT_IMAGES = 20;

/**
 * The most decoded bytes one image may carry: 3,750,000, whose base64 is
 * 5,000,000 characters, the smallest per-image limit a provider serving these
 * routes documents (Anthropic on Amazon Bedrock: 5 MB base64-encoded).
 */
export const MAX_INPUT_IMAGE_BYTES = 3_750_000;

/** Why one image part cannot be carried, before anything is held. Never the image itself. */
export interface ImageInputIssue {
  /** The message, as its index in the request's message list. */
  readonly messageIndex: number;
  /** The part, as its index in that message's content; absent for the request-wide count. */
  readonly partIndex?: number;
  /** The field at fault, relative to the part. */
  readonly field: 'role' | 'source' | 'source.mediaType' | 'source.data' | 'count';
  readonly message: string;
}

/**
 * Check the image parts of a conversation against the contract's image rules
 * (Kaana ADR 0007). Kaana refuses a request with `invalid_request` and the first
 * issue BEFORE it asks Oxy for a grant; Oxy applies the same rules on its own
 * edge; a client may run it first.
 *
 *  - an image is `inline`: no component fetches an image URL, and a `url`
 *    source is never forwarded to a provider to fetch either;
 *  - its media type is one of {@link INPUT_IMAGE_MEDIA_TYPES};
 *  - its data is canonical base64 of at most {@link MAX_INPUT_IMAGE_BYTES} bytes;
 *  - it rides on a `user` or `tool` message;
 *  - the request carries at most {@link MAX_INPUT_IMAGES} of them.
 *
 * Returns the number of image parts and every issue, in message order. An issue
 * names where and why, never the bytes: an image is payload and is never echoed
 * into an error, a log line or a record.
 */
export function checkImageInput(messages: readonly InferenceMessage[]): {
  readonly images: number;
  readonly issues: ImageInputIssue[];
} {
  const issues: ImageInputIssue[] = [];
  let images = 0;
  for (const [messageIndex, message] of messages.entries()) {
    for (const [partIndex, part] of message.content.entries()) {
      if (part.type !== 'image') continue;
      images += 1;
      const at = { messageIndex, partIndex } as const;
      if (message.role !== 'user' && message.role !== 'tool') {
        issues.push({ ...at, field: 'role', message: 'an image rides on a user or tool message' });
      }
      const source = part.source;
      if (source.kind !== 'inline') {
        issues.push({
          ...at,
          field: 'source',
          message: 'an image is sent inline as base64; no image URL is fetched',
        });
        continue;
      }
      if (!(INPUT_IMAGE_MEDIA_TYPES as readonly string[]).includes(source.mediaType)) {
        issues.push({
          ...at,
          field: 'source.mediaType',
          message: `an image is one of ${INPUT_IMAGE_MEDIA_TYPES.join(', ')}`,
        });
      }
      const bytes = base64DecodedLength(source.data);
      if (bytes === undefined) {
        issues.push({ ...at, field: 'source.data', message: 'image data is canonical base64' });
      } else if (bytes > MAX_INPUT_IMAGE_BYTES) {
        issues.push({
          ...at,
          field: 'source.data',
          message: `an image is at most ${MAX_INPUT_IMAGE_BYTES} bytes`,
        });
      }
    }
  }
  if (images > MAX_INPUT_IMAGES) {
    issues.push({
      messageIndex: messages.length - 1,
      field: 'count',
      message: `a request carries at most ${MAX_INPUT_IMAGES} images`,
    });
  }
  return { images, issues };
}

const BASE64_BODY = /^[A-Za-z0-9+/]*$/;

/**
 * The decoded length of canonical base64 (RFC 4648 section 4, padded, no line
 * breaks), or `undefined` when the text is not that. Counted, never decoded.
 */
export function base64DecodedLength(data: string): number | undefined {
  if (data.length === 0 || data.length % 4 !== 0) return undefined;
  const padding = data.endsWith('==') ? 2 : data.endsWith('=') ? 1 : 0;
  if (!BASE64_BODY.test(data.slice(0, data.length - padding))) return undefined;
  return (data.length / 4) * 3 - padding;
}

/**
 * A tool call an assistant made, in the normalized form.
 *
 * `arguments` is the JSON TEXT the model emitted, not a parsed object: models
 * emit invalid JSON often enough that parsing it here would turn a recoverable
 * model mistake into a rejected message.
 */
export const inferenceToolCallSchema = z
  .object({
    id: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)),
    name: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)),
    arguments: z.string(),
  })
  .strict();

export const inferenceMessageRoleSchema = z.enum([
  'system',
  'developer',
  'user',
  'assistant',
  'tool',
]);

/**
 * One normalized message.
 *
 * The role-specific fields are refined rather than modelled as a discriminated
 * union so the shape stays the one a caller recognises from the OpenAI-style
 * dialects; the refinement is what stops `toolCallId` from riding on a user
 * message, where every provider would silently ignore it.
 */
export const inferenceMessageSchema = z
  .object({
    role: inferenceMessageRoleSchema,
    content: z.array(inferenceContentPartSchema),
    /** Participant name, where the dialect supports naming participants. */
    name: z.string().check(maxUtf16Length(128)).optional(),
    /** The tool call this message answers. Required on, and only on, `tool`. */
    toolCallId: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)).optional(),
    /** Tool calls the assistant made. Only on `assistant`. */
    toolCalls: z.array(inferenceToolCallSchema).optional(),
  })
  .strict()
  .superRefine((message, ctx) => {
    if (message.role === 'tool' && message.toolCallId === undefined) {
      ctx.addIssue({
        code: 'custom',
        path: ['toolCallId'],
        message: 'a tool message must name the tool call it answers',
      });
    }

    if (message.role !== 'tool' && message.toolCallId !== undefined) {
      ctx.addIssue({
        code: 'custom',
        path: ['toolCallId'],
        message: 'only a tool message answers a tool call',
      });
    }

    if (message.role !== 'assistant' && message.toolCalls !== undefined) {
      ctx.addIssue({
        code: 'custom',
        path: ['toolCalls'],
        message: 'only an assistant message makes tool calls',
      });
    }

    // Same rule as `toolCalls`, for the same reason: only the assistant can
    // decline, so a refusal on any other role is a part every provider would
    // silently ignore — and one a renderer might not.
    if (message.role !== 'assistant') {
      for (const [index, part] of message.content.entries()) {
        if (part.type === 'refusal') {
          ctx.addIssue({
            code: 'custom',
            path: ['content', index, 'type'],
            message: 'only an assistant message carries a refusal',
          });
        }
      }
    }
  });

/** Sampling parameters, all optional: absent means the route's own default. */
export const samplingParametersSchema = z
  .object({
    temperature: z.number().min(0).max(2).optional(),
    topP: z.number().min(0).max(1).optional(),
    topK: z.number().int().positive().safe().optional(),
    frequencyPenalty: z.number().min(-2).max(2).optional(),
    presencePenalty: z.number().min(-2).max(2).optional(),
    /** A seed makes a request reproducible on providers that honour one. */
    seed: z.number().int().safe().optional(),
    stopSequences: z
      .array(z.string().check(minUtf16Length(1)).check(maxUtf16Length(256)))
      .max(8)
      .optional(),
  })
  .strict();

/**
 * A tool the model may call. `parameters` is a JSON Schema document, carried
 * as an opaque object: validating the customer's JSON Schema against a meta
 * schema here would reject documents providers accept.
 */
export const toolDefinitionSchema = z
  .object({
    type: z.literal('function'),
    name: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)),
    description: z.string().check(maxUtf16Length(2000)).optional(),
    parameters: z.record(z.string(), z.unknown()),
    /** Ask the provider to enforce the schema, where it supports enforcement. */
    strict: z.boolean().optional(),
  })
  .strict();

/** Whether, and which, tool the model must call. */
export const toolChoiceSchema = z.union([
  z.enum(['auto', 'none', 'required']),
  z
    .object({
      type: z.literal('function'),
      name: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)),
    })
    .strict(),
]);

/** Structured-output request: free text, any JSON object, or a named schema. */
export const responseFormatSchema = z.discriminatedUnion('type', [
  z.object({ type: z.literal('text') }).strict(),
  z.object({ type: z.literal('json_object') }).strict(),
  z
    .object({
      type: z.literal('json_schema'),
      name: z.string().check(minUtf16Length(1)).check(maxUtf16Length(128)),
      schema: z.record(z.string(), z.unknown()),
      strict: z.boolean(),
    })
    .strict(),
]);

/**
 * The caller's reasoning control. Strict, like every leaf: a provider-specific
 * knob (`budget_tokens`, `summary`) is refused rather than silently dropped.
 *
 * Oxy forwards it only after checking the resolved model advertises the effort
 * in its catalogue `reasoningEfforts`; Kaana translates it per provider.
 */
export const inferenceReasoningSchema = z.object({ effort: reasoningEffortSchema }).strict();

/** Parameters for text-to-speech, preserved in the signed request. */
export const inferenceSpeechParametersSchema = z
  .object({
    voice: z.string().check(minUtf16Length(1)).check(maxUtf16Length(64)),
    responseFormat: z.enum(['mp3', 'opus', 'aac', 'flac', 'wav', 'pcm']),
    speed: z.number().min(0.25).max(4).optional(),
  })
  .strict();

/**
 * Parameters for a file transcription, preserved in the signed request
 * (contract set 4.5.0). `language` is the caller's statement of what is spoken,
 * in the tag the model's catalogue `audio.languages` declares; absent lets the
 * model detect it. An adapter whose provider takes no language refuses one
 * rather than dropping it.
 */
export const inferenceTranscriptionParametersSchema = z
  .object({
    language: languageTagSchema.optional(),
    /** Vocabulary or context, never an instruction (contract set 5.0.0). */
    prompt: z.string().check(maxUtf16Length(4096)).optional(),
  })
  .strict();

/**
 * Spoken output from a CONVERSATIONAL model (contract set 3.2.0) — an audio chat
 * model answering in its own voice, as opposed to `speech`, which reads given
 * text aloud. Presence is the request: a chat request carrying this asks for
 * the answer as audio AND its transcript, which is the only combination the
 * audio chat models produce (OpenAI's `modalities: ["text", "audio"]`).
 *
 * The audio streams as `audio` events and the words as `delta` events on the
 * `output_audio_transcript` channel — never as `output_text`, because a
 * transcript of speech the customer is already hearing is not a second answer.
 * `pcm` is the only format an audio chat model can STREAM; the refinement on the
 * envelope holds that rather than leaving a provider to refuse it mid-flight.
 */
export const inferenceAudioOutputParametersSchema = z
  .object({
    voice: z.string().check(minUtf16Length(1)).check(maxUtf16Length(64)),
    format: z.enum(['wav', 'mp3', 'flac', 'opus', 'pcm']),
  })
  .strict();

/**
 * Customer-supplied cost-attribution labels, echoed on the generation record.
 *
 * Bounded in both directions because they are stored and read back: sixteen
 * keys is a team/feature/environment tag set, not a place to smuggle a payload.
 *
 * Moved from the Oxy edge (`packages/api/src/schemas/inferenceEdge.schemas.ts`).
 */
export const requestLabelsSchema = z
  .record(z.string(), z.string().check(maxUtf16Length(256)))
  .refine(
    (labels) => Object.keys(labels).length <= 16,
    'at most 16 labels may be attached to one request',
  );

export type InferenceSpeechParameters = z.infer<typeof inferenceSpeechParametersSchema>;
export type InferenceTranscriptionParameters = z.infer<
  typeof inferenceTranscriptionParametersSchema
>;
export type InferenceAudioOutputParameters = z.infer<typeof inferenceAudioOutputParametersSchema>;
export type InferenceContentSource = z.infer<typeof inferenceContentSourceSchema>;
export type InferenceContentPart = z.infer<typeof inferenceContentPartSchema>;
export type InferenceToolCall = z.infer<typeof inferenceToolCallSchema>;
export type InferenceMessageRole = z.infer<typeof inferenceMessageRoleSchema>;
export type InferenceMessage = z.infer<typeof inferenceMessageSchema>;
export type SamplingParameters = z.infer<typeof samplingParametersSchema>;
export type InferenceReasoning = z.infer<typeof inferenceReasoningSchema>;
export type ToolDefinition = z.infer<typeof toolDefinitionSchema>;
export type ToolChoice = z.infer<typeof toolChoiceSchema>;
export type ResponseFormat = z.infer<typeof responseFormatSchema>;
export type RequestLabels = z.infer<typeof requestLabelsSchema>;
