/** `POST /v1/audio/speech` (whole and streamed) and `POST /v1/audio/transcriptions`. */

import {
  type AudioInputMediaType,
  type AudioMediaType,
  audioInputMediaTypeSchema,
  KAANA_MODEL_HEADER,
  KAANA_USAGE_HEADER,
  MAX_INFERENCE_AUDIO_BYTES,
  MAX_TRANSCRIPTION_FILE_BYTES,
  type ModelReference,
  modelReferenceSchema,
  type SpeechRequest,
  STREAMABLE_SPEECH_FORMATS,
  speechMediaType,
  speechRequestSchema,
  speechStreamEventSchema,
  type Transcription,
  transcriptionJsonRequestSchema,
  transcriptionRequestSchema,
  transcriptionSchema,
  type Usage,
  usageSchema,
} from '@kaana.ai/contracts';
import { concatBytes, decodeBase64, encodeBase64 } from './base64.js';
import { clientRefusal, fromPublicError, KaanaProtocolError } from './errors.js';
import { cutOff, frameJson, readFrames, streamingBody } from './events.js';
import {
  bodyReadFailure,
  mediaTypeOf,
  type RequestOptions,
  type Sent,
  type Transport,
  validate,
} from './http.js';
import { deferred, StreamHandle } from './stream.js';

export interface SpeechResult {
  audio: Uint8Array;
  /** 'audio/mpeg' | 'audio/wav' | 'audio/ogg' | 'audio/aac' | 'audio/flac' | 'audio/pcm' */
  mediaType: AudioMediaType;
  /** The revision that actually served the request. */
  model: ModelReference;
  requestId: string;
  usage: Usage;
}

/**
 * Audio in order, as soon as it is produced (`stream_format: 'sse'`, so a
 * failure mid-stream arrives typed).
 */
export interface SpeechStream extends AsyncIterable<Uint8Array> {
  readonly mediaType: Promise<AudioMediaType>;
  readonly requestId: Promise<string>;
  toReadableStream(): ReadableStream<Uint8Array>;
  /** Model, usage and requestId at the end; rejects with KaanaError. */
  readonly done: Promise<Omit<SpeechResult, 'audio'>>;
  abort(reason?: unknown): void;
}

export interface TranscriptionRequest {
  model: ModelReference;
  /** A Blob is uploaded as multipart; bytes are sent as JSON with inline base64. */
  file: Blob | Uint8Array;
  mediaType: AudioInputMediaType;
  /**
   * The spoken language as the app's own BCP 47 tag (`es-AR`), when known.
   * Kaana sends the provider its spelling of it, or none for the model to
   * detect; it is never refused.
   */
  language?: string;
  /** A hint (vocabulary, names), used where the model takes one. */
  prompt?: string;
}

/* -------------------------------------------------------------------------- */
/*  Speech                                                                     */
/* -------------------------------------------------------------------------- */

export async function speech(
  transport: Transport,
  request: SpeechRequest,
  options: RequestOptions,
): Promise<SpeechResult> {
  if (request.stream === true || request.stream_format !== undefined) {
    throw clientRefusal('stream', 'audio.speech returns whole audio; use audio.speechStream');
  }
  const body = checked(request);
  const sent = await transport.send({
    method: 'POST',
    path: '/audio/speech',
    body,
    accept: 'audio/*',
    options,
  });
  const requestId = sent.requestId;
  const mediaType = speechMediaType(request.response_format ?? 'mp3');
  try {
    if (mediaTypeOf(sent.response) !== mediaType) {
      throw new KaanaProtocolError(
        `Speech in ${request.response_format ?? 'mp3'} was answered with ${mediaTypeOf(sent.response) ?? 'no Content-Type'}.`,
        requestId,
      );
    }
    const model = validate(
      sent.response.headers.get(KAANA_MODEL_HEADER),
      modelReferenceSchema,
      requestId,
      `${KAANA_MODEL_HEADER} header`,
    );
    const usage = parseUsageHeader(sent.response.headers.get(KAANA_USAGE_HEADER), requestId);
    const audio = await readAudio(sent);
    if (audio.byteLength === 0) {
      throw new KaanaProtocolError('The speech response carries no audio.', requestId);
    }
    return { audio, mediaType, model, requestId, usage };
  } catch (error) {
    sent.abort();
    throw error;
  } finally {
    sent.release();
  }
}

export function speechStream(
  transport: Transport,
  request: SpeechRequest,
  options: RequestOptions,
): SpeechStream {
  const format = request.response_format ?? 'mp3';
  if (!(STREAMABLE_SPEECH_FORMATS as readonly string[]).includes(format)) {
    throw clientRefusal(
      'response_format',
      `streamed speech is ${STREAMABLE_SPEECH_FORMATS.join(' or ')}; ${format} exists only whole`,
    );
  }
  const body = checked({ ...request, stream: true, stream_format: 'sse' });
  const mediaType = speechMediaType(format);
  const handle = new StreamHandle<Uint8Array, Omit<SpeechResult, 'audio'>>(async function* (
    context,
  ) {
    const sent = await transport.send({
      method: 'POST',
      path: '/audio/speech',
      body,
      accept: 'text/event-stream',
      options: { ...options, signal: context.signal },
      stream: true,
    });
    const requestId = sent.requestId;
    context.setRequestId(requestId);
    let output = false;
    for await (const frame of readFrames(sent, () => output)) {
      const event = validate(
        frameJson(frame, requestId),
        speechStreamEventSchema,
        requestId,
        'speech event',
      );
      if (frame.event !== event.type) {
        throw new KaanaProtocolError(
          `The stream framed a ${event.type} event as ${frame.event || 'unnamed'}.`,
          requestId,
        );
      }
      switch (event.type) {
        case 'speech.audio.delta':
          output = true;
          context.markOutput();
          yield decodeBase64(event.audio);
          break;
        case 'speech.audio.done':
          if (event.requestId !== requestId) {
            throw new KaanaProtocolError('The stream ended for another request.', requestId);
          }
          return { mediaType, model: event.model, requestId, usage: event.usage };
        case 'error':
          throw fromPublicError(event.error, null, output);
      }
    }
    throw cutOff(requestId);
  }, options.signal);

  const mediaTypePromise = deferred<AudioMediaType>();
  mediaTypePromise.resolve(mediaType);
  return {
    mediaType: mediaTypePromise.promise,
    get requestId() {
      return handle.requestId;
    },
    get done() {
      return handle.final;
    },
    abort: (reason?: unknown) => handle.abort(reason),
    [Symbol.asyncIterator]: () => handle[Symbol.asyncIterator](),
    toReadableStream(): ReadableStream<Uint8Array> {
      const iterator = handle[Symbol.asyncIterator]();
      return new ReadableStream<Uint8Array>({
        async pull(controller) {
          try {
            const next = await iterator.next();
            if (next.done) controller.close();
            else controller.enqueue(next.value);
          } catch (error) {
            controller.error(error);
          }
        },
        async cancel() {
          await iterator.return?.();
        },
      });
    },
  };
}

function checked(request: SpeechRequest): string {
  const parsed = speechRequestSchema.safeParse(request);
  if (!parsed.success) {
    const issue = parsed.error.issues[0];
    throw clientRefusal(String(issue?.path[0] ?? 'request'), issue?.message ?? 'invalid request');
  }
  return JSON.stringify(request);
}

/** `unit=quantity,unit=quantity`, each unit once. */
export function parseUsageHeader(header: string | null, requestId: string): Usage {
  if (header === null) {
    throw new KaanaProtocolError(`The response carries no ${KAANA_USAGE_HEADER}.`, requestId);
  }
  const units =
    header.trim().length === 0
      ? []
      : header.split(',').map((pair) => {
          const [unit, quantity, extra] = pair.trim().split('=');
          if (extra !== undefined || quantity === undefined || !/^\d+$/.test(quantity)) {
            throw new KaanaProtocolError(`${KAANA_USAGE_HEADER} is malformed.`, requestId);
          }
          return { unit, quantity: Number(quantity) };
        });
  return validate(units, usageSchema, requestId, `${KAANA_USAGE_HEADER} header`);
}

async function readAudio(sent: Sent): Promise<Uint8Array> {
  const body = sent.response.body;
  if (body === null || typeof body.getReader !== 'function') {
    // Runtimes without a streaming body still give the whole buffer.
    let buffer: ArrayBuffer;
    try {
      buffer = await sent.response.arrayBuffer();
    } catch (cause) {
      throw bodyReadFailure(sent, cause);
    }
    if (buffer.byteLength > MAX_INFERENCE_AUDIO_BYTES) {
      throw new KaanaProtocolError('The speech audio exceeds its size limit.', sent.requestId);
    }
    return new Uint8Array(buffer);
  }
  const reader = streamingBody(sent).getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  let completed = false;
  try {
    while (true) {
      let next: Awaited<ReturnType<typeof reader.read>>;
      try {
        next = await reader.read();
      } catch (cause) {
        throw bodyReadFailure(sent, cause);
      }
      if (next.done) {
        completed = true;
        break;
      }
      size += next.value.byteLength;
      if (size > MAX_INFERENCE_AUDIO_BYTES) {
        throw new KaanaProtocolError('The speech audio exceeds its size limit.', sent.requestId);
      }
      chunks.push(next.value);
    }
  } finally {
    if (!completed) await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
  return concatBytes(chunks);
}

/* -------------------------------------------------------------------------- */
/*  Transcription                                                              */
/* -------------------------------------------------------------------------- */

const EXTENSIONS: Record<AudioInputMediaType, string> = {
  'audio/mpeg': 'mp3',
  'audio/mp4': 'm4a',
  'audio/wav': 'wav',
  'audio/ogg': 'ogg',
  'audio/webm': 'webm',
  'audio/flac': 'flac',
  'audio/aac': 'aac',
  'audio/pcm': 'pcm',
};

export async function transcribe(
  transport: Transport,
  request: TranscriptionRequest,
  options: RequestOptions,
): Promise<Transcription> {
  const fields = {
    model: request.model,
    ...(request.language === undefined ? {} : { language: request.language }),
    ...(request.prompt === undefined ? {} : { prompt: request.prompt }),
  };
  const parsedFields = transcriptionRequestSchema.safeParse(fields);
  if (!parsedFields.success) {
    const issue = parsedFields.error.issues[0];
    throw clientRefusal(String(issue?.path[0] ?? 'request'), issue?.message ?? 'invalid request');
  }
  if (!audioInputMediaTypeSchema.safeParse(request.mediaType).success) {
    throw clientRefusal('mediaType', `${String(request.mediaType)} is not an accepted audio type`);
  }
  const isBlob = typeof Blob !== 'undefined' && request.file instanceof Blob;
  const size = isBlob ? (request.file as Blob).size : (request.file as Uint8Array).byteLength;
  if (size === 0) throw clientRefusal('file', 'the audio file is empty');
  if (size > MAX_TRANSCRIPTION_FILE_BYTES) {
    throw clientRefusal('file', `the audio file exceeds ${MAX_TRANSCRIPTION_FILE_BYTES} bytes`);
  }

  let body: string | FormData;
  if (isBlob) {
    const blob = request.file as Blob;
    const part =
      blob.type === request.mediaType ? blob : new Blob([blob], { type: request.mediaType });
    const form = new FormData();
    form.append('file', part, `audio.${EXTENSIONS[request.mediaType]}`);
    for (const [name, value] of Object.entries(fields)) form.append(name, value);
    body = form;
  } else {
    const json = {
      ...fields,
      audio: { mediaType: request.mediaType, data: encodeBase64(request.file as Uint8Array) },
    };
    const parsedJson = transcriptionJsonRequestSchema.safeParse(json);
    if (!parsedJson.success) {
      const issue = parsedJson.error.issues[0];
      throw clientRefusal(issue?.path.map(String).join('.') || 'file', issue?.message ?? 'invalid');
    }
    body = JSON.stringify(json);
  }

  const { value } = await transport.json(
    { method: 'POST', path: '/audio/transcriptions', body, accept: 'application/json', options },
    transcriptionSchema,
  );
  return value;
}
