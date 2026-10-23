/** The `Kaana` client (Kaana ADR 0002, "Superficie del SDK"). */

import {
  type ChatCompletion,
  type ChatCompletionChunk,
  type ChatCompletionsRequest,
  chatCompletionSchema,
  chatCompletionsRequestSchema,
  type DecisionRequest,
  type DecisionSuccess,
  decisionAnswersMatch,
  decisionRequestSchema,
  decisionSuccessSchema,
  type EmbeddingsRequest,
  type EmbeddingsResponse,
  embeddingsRequestSchema,
  embeddingsResponseSchema,
  type GenerationRecord,
  generationRecordSchema,
  type ImageGenerations,
  type ImageGenerationsRequest,
  imageGenerationsRequestSchema,
  imageGenerationsSchema,
  type Model,
  type ModelReference,
  modelListSchema,
  modelReferenceSchema,
  modelSchema,
  type PowerLevelEntry,
  powerLevelListSchema,
  type Response,
  type ResponseStreamEvent,
  type ResponsesRequest,
  responseSchema,
  responsesRequestSchema,
  type SpeechRequest,
  type Transcription,
} from '@kaana.ai/contracts';
import {
  type SpeechResult,
  type SpeechStream,
  speech,
  speechStream,
  type TranscriptionRequest,
  transcribe,
} from './audio.js';
import { chatStream } from './chat.js';
import { clientRefusal, KaanaProtocolError } from './errors.js';
import {
  type KaanaOptions,
  type KaanaTransportOptions,
  type RealtimeConnectOptions,
  type RequestOptions,
  type SafeParse,
  Transport,
} from './http.js';
import { connectRealtime, type RealtimeConnectRequest, type RealtimeSession } from './realtime.js';
import { responsesStream } from './responses.js';
import type { KaanaStream } from './stream.js';

/** Validate a request before sending it; the original object is what is sent. */
function body<T>(request: T, schema: { safeParse(value: unknown): SafeParse<unknown> }): string {
  const parsed = schema.safeParse(request);
  if (!parsed.success) {
    const issue = parsed.error.issues[0] as { path: readonly PropertyKey[]; message?: string };
    throw clientRefusal(
      issue?.path.map(String).join('.') || 'request',
      issue?.message ?? 'the request does not satisfy @kaana.ai/contracts',
    );
  }
  return JSON.stringify(request);
}

function modelPath(id: ModelReference): string {
  if (!modelReferenceSchema.safeParse(id).success) {
    throw clientRefusal('id', 'a model reference is publisher/model[@revision]');
  }
  return id
    .split('/')
    .map((segment) => encodeURIComponent(segment))
    .join('/');
}

export class Kaana {
  readonly #transport: Transport;

  readonly models: {
    list(options?: RequestOptions): Promise<Model[]>;
    get(id: ModelReference, options?: RequestOptions): Promise<Model>;
  };
  readonly powerLevels: {
    list(options?: RequestOptions): Promise<PowerLevelEntry[]>;
  };
  readonly responses: {
    create(request: ResponsesRequest, options?: RequestOptions): Promise<Response>;
    stream(
      request: ResponsesRequest,
      options?: RequestOptions,
    ): KaanaStream<ResponseStreamEvent, Response>;
  };
  readonly chat: {
    completions: {
      create(request: ChatCompletionsRequest, options?: RequestOptions): Promise<ChatCompletion>;
      stream(
        request: ChatCompletionsRequest,
        options?: RequestOptions,
      ): KaanaStream<ChatCompletionChunk, ChatCompletion>;
    };
  };
  readonly embeddings: {
    create(request: EmbeddingsRequest, options?: RequestOptions): Promise<EmbeddingsResponse>;
  };
  readonly audio: {
    speech(request: SpeechRequest, options?: RequestOptions): Promise<SpeechResult>;
    speechStream(request: SpeechRequest, options?: RequestOptions): SpeechStream;
    transcriptions: {
      create(request: TranscriptionRequest, options?: RequestOptions): Promise<Transcription>;
    };
  };
  readonly images: {
    generate(request: ImageGenerationsRequest, options?: RequestOptions): Promise<ImageGenerations>;
  };
  readonly decisions: {
    create(request: DecisionRequest, options?: RequestOptions): Promise<DecisionSuccess>;
  };
  readonly generations: {
    get(id: string, options?: RequestOptions): Promise<GenerationRecord>;
    getByIdempotencyKey(key: string, options?: RequestOptions): Promise<GenerationRecord>;
  };
  readonly realtime: {
    connect(
      request: RealtimeConnectRequest,
      options?: RealtimeConnectOptions,
    ): Promise<RealtimeSession>;
  };

  constructor(options: KaanaOptions | KaanaTransportOptions) {
    const transport = new Transport(options);
    this.#transport = transport;
    const get = async <T>(
      path: string,
      schema: { safeParse(value: unknown): SafeParse<T> },
      options: RequestOptions = {},
      query?: Record<string, string>,
    ): Promise<T> =>
      (
        await transport.json(
          { method: 'GET', path, accept: 'application/json', options, ...(query ? { query } : {}) },
          schema,
        )
      ).value;
    const post = async <T>(
      path: string,
      json: string,
      schema: { safeParse(value: unknown): SafeParse<T> },
      options: RequestOptions = {},
      dialect: 'public' | 'chat' = 'public',
    ): Promise<T> =>
      (
        await transport.json(
          { method: 'POST', path, body: json, accept: 'application/json', options, dialect },
          schema,
        )
      ).value;

    this.models = {
      list: async (options) => (await get('/models', modelListSchema, options)).data,
      get: async (id, options) => get(`/models/${modelPath(id)}`, modelSchema, options),
    };
    this.powerLevels = {
      list: async (options) => (await get('/power-levels', powerLevelListSchema, options)).data,
    };
    this.responses = {
      create: async (request, options) => {
        if (request.stream === true) {
          throw clientRefusal('stream', 'responses.create returns whole; use responses.stream');
        }
        return post('/responses', body(request, responsesRequestSchema), responseSchema, options);
      },
      stream: (request, options = {}) =>
        responsesStream(
          transport,
          body({ ...request, stream: true }, responsesRequestSchema),
          options,
        ),
    };
    this.chat = {
      completions: {
        create: async (request, options) => {
          if (request.stream === true) {
            throw clientRefusal(
              'stream',
              'chat.completions.create returns whole; use chat.completions.stream',
            );
          }
          return post(
            '/chat/completions',
            body(request, chatCompletionsRequestSchema),
            chatCompletionSchema,
            options,
            'chat',
          );
        },
        stream: (request, options = {}) =>
          chatStream(
            transport,
            body({ ...request, stream: true }, chatCompletionsRequestSchema),
            options,
          ),
      },
    };
    this.embeddings = {
      create: async (request, options) =>
        post(
          '/embeddings',
          body(request, embeddingsRequestSchema),
          embeddingsResponseSchema,
          options,
        ),
    };
    this.audio = {
      speech: (request, options = {}) => speech(transport, request, options),
      speechStream: (request, options = {}) => speechStream(transport, request, options),
      transcriptions: {
        create: (request, options = {}) => transcribe(transport, request, options),
      },
    };
    this.images = {
      generate: async (request, options) =>
        post(
          '/images/generations',
          body(request, imageGenerationsRequestSchema),
          imageGenerationsSchema,
          options,
        ),
    };
    this.decisions = {
      create: async (request, options) => {
        const sent = body(request, decisionRequestSchema);
        const success = await post('/decisions', sent, decisionSuccessSchema, options);
        // A schema-valid answer can still answer a different question: bind it
        // to this request, and to the pinned revision when one was named.
        if (
          !decisionAnswersMatch(request, success.data) ||
          (request.model !== undefined && success.model !== request.model)
        ) {
          throw new KaanaProtocolError(
            'The decisions do not answer the questions or the model this request named.',
            success.requestId,
          );
        }
        return success;
      },
    };
    this.generations = {
      get: async (id, options) => {
        if (typeof id !== 'string' || id.length === 0 || id.length > 128) {
          throw clientRefusal('id', 'a generation id is 1 to 128 characters');
        }
        return get(`/generations/${encodeURIComponent(id)}`, generationRecordSchema, options);
      },
      getByIdempotencyKey: async (key, options) => {
        if (typeof key !== 'string' || key.length === 0 || key.length > 255) {
          throw clientRefusal('idempotency_key', 'an idempotency key is 1 to 255 characters');
        }
        return get('/generations', generationRecordSchema, options, { idempotency_key: key });
      },
    };
    this.realtime = {
      connect: (request, options = {}) => connectRealtime(transport, request, options),
    };
  }

  /** The origin and version this client speaks to. */
  get baseURL(): string {
    return this.#transport.baseURL;
  }
}
