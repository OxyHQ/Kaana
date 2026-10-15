/**
 * Generates `packages/contracts/openapi.json`: the OpenAPI 3.1 description of
 * `https://kaana.ai/v1`, for clients in languages other than TypeScript (Kaana
 * ADR 0002, decision 10).
 *
 * Every component is `z.toJSONSchema` of a schema exported from the PUBLIC
 * surface (`packages/contracts/src/index.ts`), so the document cannot describe
 * a shape the TypeScript contract does not have, and it never reaches into
 * `/internal`. The paths below only say which component each endpoint reads
 * and writes. JSON Schema cannot express Zod's refinements (cross-field rules),
 * so the Zod source stays the authority; this is its projection.
 *
 * Deterministic: CI regenerates and fails on any difference.
 */
import { writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { z } from 'zod';

import * as contract from '../../packages/contracts/src/index.ts';

const OUTPUT = resolve(import.meta.dirname, '..', '..', 'packages', 'contracts', 'openapi.json');

const isZodSchema = (value) =>
  typeof value === 'object' &&
  value !== null &&
  typeof value._zod === 'object' &&
  typeof value.safeParse === 'function';

/** `responsesRequestSchema` → `ResponsesRequest`. */
const componentName = (exportName) =>
  exportName.replace(/Schema$/, '').replace(/^./, (first) => first.toUpperCase());

/* -------------------------------------------------------------------------- */
/*  Components                                                                */
/* -------------------------------------------------------------------------- */

/**
 * Zod's JSON Schema writes a number's bounds from its LAST check, so
 * `.int().nonnegative().safe()` comes out with the safe-integer floor instead
 * of 0. Recompute them as the strongest bound any check states.
 */
function numberBounds(zodSchema, jsonSchema) {
  const def = zodSchema._zod.def;
  if (def.type !== 'number') return;
  let lower;
  let upper;
  const tighten = (current, value, exclusive, stronger) =>
    current === undefined ||
    stronger(value, current.value) ||
    (value === current.value && exclusive && !current.exclusive)
      ? { value, exclusive }
      : current;
  for (const check of def.checks ?? []) {
    const spec = check._zod.def;
    if (spec.check === 'greater_than') {
      lower = tighten(lower, spec.value, !spec.inclusive, (a, b) => a > b);
    } else if (spec.check === 'less_than') {
      upper = tighten(upper, spec.value, !spec.inclusive, (a, b) => a < b);
    } else if (spec.check === 'number_format' && spec.format === 'safeint') {
      lower = tighten(lower, Number.MIN_SAFE_INTEGER, false, (a, b) => a > b);
      upper = tighten(upper, Number.MAX_SAFE_INTEGER, false, (a, b) => a < b);
    }
  }
  for (const key of ['minimum', 'maximum', 'exclusiveMinimum', 'exclusiveMaximum']) {
    delete jsonSchema[key];
  }
  if (lower !== undefined)
    jsonSchema[lower.exclusive ? 'exclusiveMinimum' : 'minimum'] = lower.value;
  if (upper !== undefined)
    jsonSchema[upper.exclusive ? 'exclusiveMaximum' : 'maximum'] = upper.value;
}

const registry = z.registry();
const exported = Object.entries(contract)
  .filter(([, value]) => isZodSchema(value))
  .sort(([a], [b]) => (a < b ? -1 : 1));
const names = new Set();
for (const [exportName, schema] of exported) {
  const id = componentName(exportName);
  if (names.has(id)) throw new Error(`two public schemas map to component ${id}`);
  names.add(id);
  registry.add(schema, { id });
}

// `io: 'input'`: a request body is described as a client may send it (a field
// with a default may be omitted); a response is described no narrower than
// Kaana may write it.
const { schemas } = z.toJSONSchema(registry, {
  target: 'draft-2020-12',
  io: 'input',
  unrepresentable: 'throw',
  uri: (id) => `#/components/schemas/${id}`,
  override: ({ zodSchema, jsonSchema }) => numberBounds(zodSchema, jsonSchema),
});
// A component is addressed by its key; a fragment-only `$id` is not a valid
// 2020-12 identifier, and `$schema` is the document's, not the component's.
for (const schema of Object.values(schemas)) {
  delete schema.$schema;
  delete schema.$id;
}

const ref = (exportName) => {
  const id = componentName(exportName);
  if (!(id in schemas)) throw new Error(`no public component ${id} for ${exportName}`);
  return { $ref: `#/components/schemas/${id}` };
};

/* -------------------------------------------------------------------------- */
/*  Paths                                                                     */
/* -------------------------------------------------------------------------- */

const json = (exportName) => ({ 'application/json': { schema: ref(exportName) } });
/**
 * A server-sent event stream. OpenAPI 3.1 has no item schema for a stream (3.2
 * adds `itemSchema`), so each `data:` frame's schema rides in an extension.
 */
const sse = (exportName, description) => ({
  'text/event-stream': {
    schema: { type: 'string', description },
    'x-kaana-event': ref(exportName),
  },
});
const headers = {
  [contract.KAANA_REQUEST_ID_HEADER]: {
    description: 'The request id; quote it when reporting a problem.',
    schema: { type: 'string' },
  },
  [contract.KAANA_CONTRACT_VERSION_HEADER]: {
    description: 'The contract set that answered.',
    schema: { type: 'string' },
  },
};
const errors = (exportName = 'publicErrorSchema') => ({
  default: {
    description:
      'A failure decided before the first byte. After it, a stream ends with its terminal error event instead.',
    headers,
    content: json(exportName),
  },
});
const idempotencyKey = {
  name: 'Idempotency-Key',
  in: 'header',
  required: false,
  description: 'Makes a retried request return the original generation instead of running twice.',
  schema: { type: 'string', minLength: 1, maxLength: 255 },
};
const onBehalfOf = {
  name: 'X-Oxy-User-Id',
  in: 'header',
  required: false,
  description: 'Delegated end user, attribution only; honoured only with an Oxy service token.',
  schema: { type: 'string', minLength: 1, maxLength: 64 },
};
const post = (summary, request, ok, extra = {}) => ({
  post: {
    summary,
    parameters: [idempotencyKey, onBehalfOf],
    requestBody: { required: true, content: request },
    responses: { 200: { description: 'OK', headers, content: ok }, ...errors(extra.error) },
  },
});

const paths = {
  '/models': {
    get: {
      summary: 'List the models the credential may use, with prices and capabilities.',
      responses: {
        200: { description: 'OK', headers, content: json('modelListSchema') },
        ...errors(),
      },
    },
  },
  '/models/{publisher}/{model}': {
    get: {
      summary: 'One model.',
      parameters: [
        { name: 'publisher', in: 'path', required: true, schema: { type: 'string' } },
        { name: 'model', in: 'path', required: true, schema: { type: 'string' } },
      ],
      responses: { 200: { description: 'OK', headers, content: json('modelSchema') }, ...errors() },
    },
  },
  '/power-levels': {
    get: {
      summary: 'The power levels the credential may use and the model lines each can choose.',
      responses: {
        200: { description: 'OK', headers, content: json('powerLevelListSchema') },
        ...errors(),
      },
    },
  },
  '/responses': post('Generate a response (native dialect).', json('responsesRequestSchema'), {
    ...json('responseSchema'),
    ...sse('responseStreamEventSchema', 'With `stream: true`.'),
  }),
  '/chat/completions': post(
    'Generate a chat completion (OpenAI-compatible).',
    json('chatCompletionsRequestSchema'),
    {
      ...json('chatCompletionSchema'),
      ...sse(
        'chatCompletionChunkSchema',
        'With `stream: true`: chunks, then `data: [DONE]`; a failure is a `chatCompletionError` frame.',
      ),
    },
    { error: 'chatCompletionErrorSchema' },
  ),
  '/embeddings': post(
    'Embed text (OpenAI-compatible).',
    json('embeddingsRequestSchema'),
    json('embeddingsResponseSchema'),
  ),
  '/audio/speech': post('Read text aloud.', json('speechRequestSchema'), {
    'audio/mpeg': { schema: { type: 'string', contentMediaType: 'audio/mpeg' } },
    'audio/ogg': { schema: { type: 'string', contentMediaType: 'audio/ogg' } },
    'audio/aac': { schema: { type: 'string', contentMediaType: 'audio/aac' } },
    'audio/flac': { schema: { type: 'string', contentMediaType: 'audio/flac' } },
    'audio/wav': { schema: { type: 'string', contentMediaType: 'audio/wav' } },
    'audio/pcm': { schema: { type: 'string', contentMediaType: 'audio/pcm' } },
    ...sse('speechStreamEventSchema', 'With `stream: true` and `stream_format: "sse"`.'),
  }),
  '/audio/transcriptions': post(
    'Transcribe an uploaded audio file (synchronous), as multipart or as JSON with inline base64.',
    {
      'multipart/form-data': {
        schema: {
          allOf: [
            ref('transcriptionRequestSchema'),
            {
              type: 'object',
              required: ['file'],
              properties: {
                file: {
                  type: 'string',
                  contentMediaType: 'application/octet-stream',
                  description: `At most ${contract.MAX_TRANSCRIPTION_FILE_BYTES} bytes, of one of the AudioInputMediaType types.`,
                },
              },
            },
          ],
        },
      },
      'application/json': { schema: ref('transcriptionJsonRequestSchema') },
    },
    json('transcriptionSchema'),
  ),
  '/images/generations': post(
    'Generate images (OpenAI-compatible).',
    json('imageGenerationsRequestSchema'),
    json('imageGenerationsSchema'),
  ),
  '/decisions': post(
    'Answer structured questions about a state.',
    json('decisionRequestSchema'),
    json('decisionSuccessSchema'),
  ),
  '/generations/{id}': {
    get: {
      summary: 'The usage and cost record of one generation.',
      parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
      responses: {
        200: { description: 'OK', headers, content: json('generationRecordSchema') },
        ...errors(),
      },
    },
  },
  '/generations': {
    get: {
      summary: 'The record of the generation an idempotency key produced.',
      parameters: [
        {
          name: 'idempotency_key',
          in: 'query',
          required: true,
          schema: { type: 'string', minLength: 1, maxLength: 255 },
        },
        onBehalfOf,
      ],
      responses: {
        200: { description: 'OK', headers, content: json('generationRecordSchema') },
        ...errors(),
      },
    },
  },
  '/realtime': {
    get: {
      summary:
        'Open a realtime session over WebSocket. The first frame is RealtimeSessionOpen (or session.resume); every frame is one JSON message.',
      parameters: [
        { name: 'model', in: 'query', required: true, schema: ref('modelReferenceSchema') },
      ],
      responses: {
        101: { description: 'Switching Protocols: the WebSocket is open.' },
        ...errors(),
      },
      'x-kaana-websocket': {
        open: ref('realtimeSessionOpenSchema'),
        client: ref('realtimeClientCommandSchema'),
        server: ref('realtimePublicServerEventSchema'),
      },
    },
  },
};

const document = {
  openapi: '3.1.0',
  info: {
    title: 'Kaana API',
    version: contract.INFERENCE_CONTRACT_VERSION,
    description:
      'Kaana inference API. Generated from @kaana.ai/contracts; the Zod source is the authority. Accounts, applications, oxy_sk_ keys, limits and billing are managed in Oxy Console: https://console.oxy.so',
  },
  servers: [{ url: contract.KAANA_API_BASE_URL }],
  security: [{ bearer: [] }],
  paths,
  components: {
    securitySchemes: {
      bearer: {
        type: 'http',
        scheme: 'bearer',
        description:
          'An oxy_sk_ key from Oxy Console, an Oxy service token for the kaana.ai audience, or an Oxy app session with inference:invoke.',
      },
    },
    schemas,
  },
};

writeFileSync(OUTPUT, `${JSON.stringify(document, null, 2)}\n`);
process.stdout.write(
  `wrote ${OUTPUT}\n  ${Object.keys(paths).length} paths, ${Object.keys(schemas).length} components\n`,
);
