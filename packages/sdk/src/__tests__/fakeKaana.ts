/**
 * A fake `https://kaana.ai/v1` that speaks the public wire exactly: HTTP, SSE
 * and the realtime WebSocket. It validates everything the SDK sends with the
 * contract's own Zod schemas and answers a refusal in the contract's error
 * body, so a test passes only if the SDK and the wire agree.
 */

import {
  chatCompletionsRequestSchema,
  decisionRequestSchema,
  embeddingsRequestSchema,
  imageGenerationsRequestSchema,
  INFERENCE_CONTRACT_VERSION,
  KAANA_REALTIME_BEARER_PROTOCOL_PREFIX,
  KAANA_REALTIME_SUBPROTOCOL,
  KAANA_REALTIME_USER_PROTOCOL_PREFIX,
  PUBLIC_ERROR_HTTP_STATUS,
  type PublicErrorCode,
  realtimeClientCommandSchema,
  realtimeSessionOpenSchema,
  responsesRequestSchema,
  speechRequestSchema,
  transcriptionJsonRequestSchema,
  transcriptionRequestSchema,
} from '@kaana.ai/contracts';
import type { Server, ServerWebSocket } from 'bun';
import { decodeBase64 } from '../base64.js';

export const CREDENTIAL = 'oxy_sk_test_0123456789';
export const MODEL = 'openai/gpt-5@2026-06-01';
export const SPEECH_MODEL = 'elevenlabs/eleven-multilingual@v2';
export const AT = '2026-10-15T09:41:00.000Z';
export const LATER = '2026-10-15T09:41:00.812Z';

export interface Recorded {
  method: string;
  path: string;
  search: URLSearchParams;
  headers: Headers;
  json?: unknown;
  form?: FormData;
}

type Handler = (request: Request, recorded: Recorded) => Response | Promise<Response>;

let counter = 0;
export const nextRequestId = (): string => {
  counter += 1;
  return `req_fake_${counter}`;
};

export function baseHeaders(requestId: string, extra: Record<string, string> = {}): Headers {
  return new Headers({
    'Kaana-Request-Id': requestId,
    'Kaana-Contract-Version': INFERENCE_CONTRACT_VERSION,
    ...extra,
  });
}

export function json(requestId: string, body: unknown, status = 200, extra = {}): Response {
  const headers = baseHeaders(requestId, { 'Content-Type': 'application/json', ...extra });
  return new Response(JSON.stringify(body), { status, headers });
}

export function publicError(
  code: PublicErrorCode,
  options: { retryable?: boolean; retryAfterMs?: number; param?: string; requestId?: string } = {},
) {
  const requestId = options.requestId ?? nextRequestId();
  return {
    schemaVersion: 1 as const,
    code,
    message: `fake ${code}`,
    retryable: options.retryable ?? false,
    requestId,
    ...(options.retryAfterMs === undefined ? {} : { retryAfterMs: options.retryAfterMs }),
    ...(options.param === undefined ? {} : { param: options.param }),
  };
}

export function errorResponse(
  code: PublicErrorCode,
  options: {
    retryable?: boolean;
    retryAfterMs?: number;
    param?: string;
    headers?: Record<string, string>;
  } = {},
): Response {
  const body = publicError(code, options);
  return json(body.requestId, body, PUBLIC_ERROR_HTTP_STATUS[code], options.headers ?? {});
}

/** An SSE body: `event:` (when named) and `data:` per frame; `cut` ends it without a terminal. */
export function sse(
  requestId: string,
  frames: { event?: string; data: unknown }[],
  extra: Record<string, string> = {},
): Response {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    async start(controller) {
      for (const frame of frames) {
        const data = typeof frame.data === 'string' ? frame.data : JSON.stringify(frame.data);
        const text = `${frame.event === undefined ? '' : `event: ${frame.event}\n`}data: ${data}\n\n`;
        // Split each frame in two writes so the decoder is exercised across chunks.
        const middle = Math.floor(text.length / 2);
        controller.enqueue(encoder.encode(text.slice(0, middle)));
        await Bun.sleep(1);
        controller.enqueue(encoder.encode(text.slice(middle)));
      }
      controller.close();
    },
  });
  return new Response(body, {
    headers: baseHeaders(requestId, { 'Content-Type': 'text/event-stream', ...extra }),
  });
}

function refuse(code: PublicErrorCode, param: string): Response {
  return errorResponse(code, { param });
}

function validated(schema: { safeParse(v: unknown): { success: boolean } }, value: unknown) {
  return schema.safeParse(value).success;
}

/* -------------------------------------------------------------------------- */
/*  Realtime                                                                   */
/* -------------------------------------------------------------------------- */

interface Connection {
  credential: string | null;
  user: string | null;
  model: string;
  session?: RealtimeFakeSession;
  opened: boolean;
}

export interface RealtimeFakeSession {
  requestId: string;
  sequence: number;
  log: string[];
  socket?: ServerWebSocket<Connection>;
  commands: unknown[];
  closed: boolean;
}

export const sessionConfig = {
  outputModalities: ['text'],
  inputAudioFormat: 'pcm16_24khz',
  turnDetection: { type: 'none' },
  tools: [{ type: 'function', name: 'lookup', parameters: { type: 'object' } }],
};

export class FakeKaana {
  constructor(private readonly credential = CREDENTIAL) {}
  readonly requests: Recorded[] = [];
  readonly handlers = new Map<string, Handler>();
  readonly sessions = new Map<string, RealtimeFakeSession>();
  readonly frames: unknown[] = [];
  /** When set, the realtime connection drops right after this command type is answered. */
  dropAfter: string | undefined;
  server!: Server<Connection>;

  get baseURL(): string {
    return `http://127.0.0.1:${this.server.port}/v1`;
  }

  start(): this {
    this.server = Bun.serve<Connection>({
      port: 0,
      hostname: '127.0.0.1',
      fetch: (request, server) => this.#fetch(request, server),
      websocket: {
        message: (ws, message) => this.#message(ws, message),
      },
    });
    return this;
  }

  stop(): void {
    this.server.stop(true);
  }

  on(route: string, handler: Handler): void {
    this.handlers.set(route, handler);
  }

  async #fetch(request: Request, server: Server<Connection>): Promise<Response | undefined> {
    const url = new URL(request.url);
    const path = url.pathname.replace(/^\/v1/, '');
    if (path === '/realtime') {
      const offered = (request.headers.get('Sec-WebSocket-Protocol') ?? '')
        .split(',')
        .map((value) => value.trim());
      if (!offered.includes(KAANA_REALTIME_SUBPROTOCOL)) {
        return new Response('subprotocol required', { status: 400 });
      }
      const decode = (prefix: string) => {
        const token = offered.find((value) => value.startsWith(prefix));
        return token === undefined
          ? null
          : new TextDecoder().decode(decodeBase64(token.slice(prefix.length)));
      };
      const ok = server.upgrade(request, {
        headers: { 'Sec-WebSocket-Protocol': KAANA_REALTIME_SUBPROTOCOL },
        data: {
          credential: decode(KAANA_REALTIME_BEARER_PROTOCOL_PREFIX),
          user: decode(KAANA_REALTIME_USER_PROTOCOL_PREFIX),
          model: url.searchParams.get('model') ?? '',
          opened: false,
        },
      });
      return ok ? undefined : new Response('upgrade failed', { status: 400 });
    }

    const recorded: Recorded = {
      method: request.method,
      path,
      search: url.searchParams,
      headers: request.headers,
    };
    const contentType = request.headers.get('Content-Type') ?? '';
    if (contentType.startsWith('application/json')) {
      recorded.json = await request.json();
    } else if (contentType.startsWith('multipart/form-data')) {
      recorded.form = await request.formData();
    }
    this.requests.push(recorded);

    if (request.headers.get('Authorization') !== `Bearer ${this.credential}`) {
      return errorResponse('authentication_failed');
    }
    const routeKey = `${request.method} ${path}`;
    const override =
      this.handlers.get(routeKey) ??
      [...this.handlers].find(
        ([key]) => key.endsWith('*') && routeKey.startsWith(key.slice(0, -1)),
      )?.[1];
    if (override !== undefined) return override(request, recorded);
    return this.#default(routeKey, recorded);
  }

  #default(route: string, recorded: Recorded): Response {
    const requestId = nextRequestId();
    const body = recorded.json as Record<string, unknown> | undefined;
    switch (route) {
      case 'POST /responses':
        if (!validated(responsesRequestSchema, body)) return refuse('invalid_request', 'body');
        return json(requestId, {
          schemaVersion: 1,
          requestId,
          model: MODEL,
          finishReason: 'stop',
          output: [{ role: 'assistant', content: [{ type: 'text', text: 'Hola.' }] }],
          usage: [{ unit: 'output_tokens', quantity: 2 }],
          routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
          latencyMs: 10,
        });
      case 'POST /chat/completions':
        if (!validated(chatCompletionsRequestSchema, body))
          return refuse('invalid_request', 'body');
        return json(requestId, {
          id: 'chatcmpl-1',
          object: 'chat.completion',
          created: 1760521260,
          model: MODEL,
          choices: [
            { index: 0, message: { role: 'assistant', content: 'Hola.' }, finish_reason: 'stop' },
          ],
          usage: {
            prompt_tokens: 3,
            completion_tokens: 2,
            total_tokens: 5,
            prompt_tokens_details: { cached_tokens: 0 },
            completion_tokens_details: { reasoning_tokens: 0 },
          },
        });
      case 'POST /embeddings':
        if (!validated(embeddingsRequestSchema, body)) return refuse('invalid_request', 'body');
        return json(requestId, {
          object: 'list',
          data: [{ object: 'embedding', index: 0, embedding: [0.1, 0.2] }],
          model: 'openai/text-embedding-3-small@2024-01-25',
          usage: { prompt_tokens: 2, total_tokens: 2 },
        });
      case 'POST /images/generations':
        if (!validated(imageGenerationsRequestSchema, body))
          return refuse('invalid_request', 'body');
        return json(requestId, { created: 1760521260, data: [{ b64_json: 'iVBORw0K' }] });
      case 'POST /decisions':
        if (!validated(decisionRequestSchema, body)) return refuse('invalid_request', 'body');
        return json(requestId, {
          schemaVersion: 1,
          requestId,
          model: MODEL,
          data: [
            { id: 'q1', kind: 'choice', reply: 'pets', confidence: 0.9, probabilities: [0.9, 0.1] },
          ],
          usage: [{ unit: 'input_tokens', quantity: 40 }],
          routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
        });
      case 'POST /audio/speech': {
        if (!validated(speechRequestSchema, body)) return refuse('invalid_request', 'body');
        return new Response(new Uint8Array([0x49, 0x44, 0x33, 4, 0, 0]), {
          headers: baseHeaders(requestId, {
            'Content-Type': 'audio/mpeg',
            'Kaana-Model': SPEECH_MODEL,
            'Kaana-Usage': 'characters=4,requests=1',
          }),
        });
      }
      case 'POST /audio/transcriptions': {
        let bytes: number;
        let model: unknown;
        if (recorded.form !== undefined) {
          const file = recorded.form.get('file');
          const fields = Object.fromEntries(
            [...recorded.form.entries()].filter(([name]) => name !== 'file'),
          );
          if (!(file instanceof Blob) || !validated(transcriptionRequestSchema, fields)) {
            return refuse('invalid_request', 'file');
          }
          bytes = file.size;
          model = fields.model;
        } else {
          const parsed = transcriptionJsonRequestSchema.safeParse(body);
          if (!parsed.success) return refuse('invalid_request', 'audio');
          bytes = decodeBase64(parsed.data.audio.data).byteLength;
          model = parsed.data.model;
        }
        return json(requestId, {
          schemaVersion: 1,
          requestId,
          model: `${String(model)}@2023-03-01`,
          text: `heard ${bytes} bytes`,
          usage: [{ unit: 'audio_input_milliseconds', quantity: 1200 }],
        });
      }
      default:
        return refuse('invalid_request', 'path');
    }
  }

  /* ---------------------------------------------------------------------- */

  #emit(session: RealtimeFakeSession, event: Record<string, unknown>): void {
    const encoded = JSON.stringify({
      schemaVersion: 1,
      requestId: session.requestId,
      sequence: session.sequence,
      ...event,
    });
    session.sequence += 1;
    session.log.push(encoded);
    session.socket?.send(encoded);
  }

  #message(ws: ServerWebSocket<Connection>, message: string | Buffer): void {
    const frame = JSON.parse(String(message));
    this.frames.push(frame);
    const data = ws.data;
    const refuse = (code: PublicErrorCode) => {
      const requestId = nextRequestId();
      ws.send(
        JSON.stringify({
          schemaVersion: 1,
          requestId,
          sequence: 0,
          type: 'error',
          fatal: true,
          error: publicError(code, { requestId }),
        }),
      );
      ws.close(1008);
    };

    if (!data.opened) {
      data.opened = true;
      if (data.credential !== this.credential) {
        refuse('authentication_failed');
        return;
      }
      if (frame.type === 'session.resume') {
        const session = this.sessions.get(frame.requestId);
        if (session === undefined || session.closed) {
          refuse('invalid_request');
          return;
        }
        session.socket = ws;
        data.session = session;
        for (const encoded of session.log) {
          if (JSON.parse(encoded).sequence > frame.afterSequence) ws.send(encoded);
        }
        this.#emit(session, {
          type: 'session.resumed',
          afterSequence: frame.afterSequence,
          commandId: frame.commandId,
        });
        return;
      }
      if (!realtimeSessionOpenSchema.safeParse(frame).success) {
        refuse('invalid_request');
        return;
      }
      const session: RealtimeFakeSession = {
        requestId: nextRequestId(),
        sequence: 0,
        log: [],
        socket: ws,
        commands: [],
        closed: false,
      };
      this.sessions.set(session.requestId, session);
      data.session = session;
      this.#emit(session, {
        type: 'session.created',
        model: data.model,
        kind: frame.kind,
        config: frame.config,
        limits: {
          maxDurationMs: 600_000,
          idleTimeoutMs: 60_000,
          maxInputAudioBytes: 10_000_000,
          maxOutputAudioBytes: 10_000_000,
          maxResponses: 100,
        },
        resumeWindowMs: 30_000,
        startedAt: AT,
        expiresAt: '2026-10-15T09:51:00.000Z',
      });
      return;
    }

    const session = data.session;
    if (session === undefined) return;
    if (
      !realtimeClientCommandSchema.safeParse(frame).success ||
      frame.requestId !== session.requestId
    ) {
      this.#emit(session, {
        type: 'error',
        fatal: false,
        error: publicError('invalid_request', { requestId: session.requestId }),
        commandId: frame.commandId,
      });
      return;
    }
    session.commands.push(frame);
    this.#emit(session, { type: 'command.accepted', commandId: frame.commandId, duplicate: false });
    switch (frame.type) {
      case 'response.create': {
        const output = frame.response === undefined ? 'tool' : 'text';
        this.#emit(session, { type: 'response.created', responseId: 'resp_1' });
        if (output === 'tool') {
          this.#emit(session, {
            type: 'tool_call',
            responseId: 'resp_1',
            itemId: 'item_1',
            toolCallId: 'call_1',
            name: 'lookup',
            argumentsDelta: '{"q":"tiempo"}',
            complete: true,
          });
        } else {
          this.#emit(session, {
            type: 'text.delta',
            responseId: 'resp_1',
            itemId: 'item_2',
            contentIndex: 0,
            text: 'Hace sol.',
          });
        }
        this.#emit(session, {
          type: 'response.done',
          responseId: 'resp_1',
          status: 'completed',
          finishReason: output === 'tool' ? 'tool_calls' : 'stop',
          units: [{ unit: 'output_tokens', quantity: 5 }],
          usageSource: 'provider_reported',
        });
        break;
      }
      case 'session.close':
        session.closed = true;
        this.#emit(session, {
          type: 'session.closed',
          reason: 'client_closed',
          units: [
            { unit: 'audio_input_milliseconds', quantity: 1000 },
            { unit: 'output_tokens', quantity: 5 },
          ],
          usageSource: 'provider_reported',
          closedAt: LATER,
        });
        ws.close(1000);
        return;
    }
    if (this.dropAfter === frame.type) {
      this.dropAfter = undefined;
      session.socket = undefined;
      ws.terminate();
      // Events produced while detached are buffered for the resume.
      this.#emit(session, {
        type: 'text.delta',
        responseId: 'resp_1',
        itemId: 'item_3',
        contentIndex: 0,
        text: 'mientras tanto',
      });
    }
  }
}
