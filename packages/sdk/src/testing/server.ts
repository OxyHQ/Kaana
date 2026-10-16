/**
 * The server half of `@kaana.ai/sdk/testing`: `https://kaana.ai/v1` as Kaana
 * answers it, for the requests a voice client makes, run in memory.
 *
 * It decides with Kaana's own published rules — the same schemas and the same
 * `check*` functions of `@kaana.ai/contracts` Kaana applies before a grant —
 * against a catalogue that is what Kaana serves (`catalogue.generated.ts`,
 * held to Kaana's render by a Go test). The client in front of it is the real
 * `Kaana`, so a refusal reaches the code under test as the SDK's own error
 * class, with the code and `param` production would send.
 *
 * It serves the canonical contract, not any provider's dialect: Kaana
 * translates or emulates every value the contract offers on every model
 * (Kaana ADR 0003), so what the contract accepts the fake accepts, and it
 * refuses only what the catalogue says a model lacks (a voice, an encoding, a
 * session kind, a transport, an upload format).
 */

import {
  checkRealtimeSessionOpen,
  checkTranscriptionRequest,
  KAANA_REALTIME_SUBPROTOCOL,
  KAANA_REQUEST_ID_HEADER,
  type Model,
  type ModelReference,
  modelSchema,
  type PublicErrorCode,
  type RealtimeClientCommand,
  type RealtimeServerEvent,
  type RealtimeSessionLimits,
  type RealtimeSessionOpen,
  realtimeClientCommandSchema,
  realtimePublicServerEventSchema,
  realtimeSessionOpenSchema,
  type Transcription,
  transcriptionJsonRequestSchema,
  transcriptionRequestSchema,
  type Usage,
  type UsageSource,
} from '@kaana.ai/contracts';
import { decodeBase64 } from '../base64.js';

/** What a transcription upload carries, as the fake's `transcribe` reads it. */
export interface FakeTranscriptionUpload {
  readonly model: Model;
  readonly mediaType: string;
  readonly audio: Uint8Array;
  readonly language?: string;
  readonly prompt?: string;
}

/** What the fake answers a transcription with. */
export interface FakeTranscriptionResult {
  readonly text: string;
  /** The audio's duration; it is what the transcription is billed by. */
  readonly durationMs: number;
  /** The language the model heard, as a BCP 47 tag. */
  readonly language?: string;
}

export interface FakeKaanaServerOptions {
  /** The catalogue `GET /v1/models` serves. */
  readonly models: readonly Model[];
  /** What a transcription that passed every check answers. */
  readonly transcribe?: (upload: FakeTranscriptionUpload) => FakeTranscriptionResult;
}

/** One request the fake received. */
export interface FakeRequest {
  readonly method: string;
  readonly path: string;
  readonly headers: Headers;
}

/** An event the test emits into a session: everything but its envelope. */
export type FakeServerEventInput = RealtimeServerEvent extends infer E
  ? E extends { type: string }
    ? Omit<E, 'schemaVersion' | 'requestId' | 'sequence'> & { sequence?: number }
    : never
  : never;

/** Kaana's defaults for a public session (`publicapi.defaultRealtimeLimits`). */
const DEFAULT_LIMITS: RealtimeSessionLimits = {
  maxDurationMs: 30 * 60 * 1000,
  idleTimeoutMs: 5 * 60 * 1000,
  maxInputAudioBytes: 172_800_000,
  maxOutputAudioBytes: 172_800_000,
  maxResponses: 1000,
};

const RESUME_WINDOW_MS = 60_000;

let counter = 0;
function requestId(): string {
  counter += 1;
  return `req_fake_${counter.toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
}

function publicError(code: PublicErrorCode, message: string, id: string, param?: string) {
  return {
    schemaVersion: 1 as const,
    code,
    message,
    retryable: false,
    requestId: id,
    ...(param === undefined ? {} : { param }),
  };
}

function json(status: number, body: unknown, id: string): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', [KAANA_REQUEST_ID_HEADER]: id },
  });
}

/** One open session on the fake: what it was opened with and what it received. */
export class FakeRealtimeSession {
  readonly requestId = requestId();
  readonly model: Model;
  readonly open: RealtimeSessionOpen;
  /** Every command the client sent and the session applied, in order. */
  readonly commands: RealtimeClientCommand[] = [];
  #sequence = 0;
  readonly #log: RealtimeServerEvent[] = [];
  readonly #seen = new Set<string>();
  #socket: FakeSocket | undefined;
  #closed = false;

  constructor(model: Model, open: RealtimeSessionOpen, socket: FakeSocket) {
    this.model = model;
    this.open = open;
    this.#socket = socket;
    const startedAt = new Date();
    this.#push({
      type: 'session.created',
      model: `${model.modelId}@${model.currentRevision}` as ModelReference,
      kind: open.kind,
      config: open.config,
      limits: { ...DEFAULT_LIMITS, ...open.limits },
      resumeWindowMs: RESUME_WINDOW_MS,
      startedAt: startedAt.toISOString(),
      expiresAt: new Date(startedAt.getTime() + DEFAULT_LIMITS.maxDurationMs).toISOString(),
    } as FakeServerEventInput);
  }

  /** Whether the session ended. */
  get closed(): boolean {
    return this.#closed;
  }

  /** Emit an event to the client, as the provider's answer would arrive. */
  emit(event: FakeServerEventInput): void {
    this.#push(event);
  }

  /** Drop the connection without ending the session, as a network would. */
  drop(): void {
    const socket = this.#socket;
    this.#socket = undefined;
    socket?.serverClose(1006);
  }

  /** End the session from the server's side. */
  end(reason: 'idle_timeout' | 'max_duration' | 'upstream_error', units: Usage = []): void {
    this.#finish(reason, units, 'provider_reported');
  }

  /** A command frame from the attached client. */
  receive(frame: unknown): void {
    const parsed = realtimeClientCommandSchema.safeParse(frame);
    if (!parsed.success) {
      this.#refuse(
        undefined,
        'invalid_request',
        parsed.error.issues[0]?.message ?? 'invalid command',
      );
      return;
    }
    const command = parsed.data;
    if (command.type === 'session.resume') {
      this.#refuse(
        command.commandId,
        'invalid_request',
        'session.resume is only ever the first frame of a new connection',
      );
      return;
    }
    if (this.#seen.has(command.commandId)) {
      this.#push({
        type: 'command.accepted',
        commandId: command.commandId,
        duplicate: true,
      } as FakeServerEventInput);
      return;
    }
    this.#seen.add(command.commandId);
    this.#push({
      type: 'command.accepted',
      commandId: command.commandId,
      duplicate: false,
    } as FakeServerEventInput);
    if (command.type === 'session.close') {
      this.commands.push(command);
      this.#finish('client_closed', [], 'oxy_measured');
      return;
    }
    this.commands.push(command);
  }

  /** A new connection resuming this session after `afterSequence`. */
  resume(socket: FakeSocket, afterSequence: number): void {
    this.#socket?.serverClose(1000);
    this.#socket = socket;
    for (const event of this.#log) {
      if (event.sequence > afterSequence) socket.deliver(event);
    }
    this.#push({ type: 'session.resumed', afterSequence } as FakeServerEventInput);
  }

  detach(socket: FakeSocket): void {
    if (this.#socket === socket) this.#socket = undefined;
  }

  #refuse(
    commandId: string | undefined,
    code: PublicErrorCode,
    message: string,
    param?: string,
  ): void {
    this.#push({
      type: 'error',
      fatal: false,
      ...(commandId === undefined ? {} : { commandId }),
      error: publicError(code, message, this.requestId, param),
    } as FakeServerEventInput);
  }

  #finish(reason: string, units: Usage, usageSource: UsageSource): void {
    if (this.#closed) return;
    this.#push({
      type: 'session.closed',
      reason,
      units,
      usageSource,
      closedAt: new Date().toISOString(),
    } as FakeServerEventInput);
    this.#closed = true;
    this.#socket?.serverClose(1000);
    this.#socket = undefined;
  }

  #push(input: FakeServerEventInput): void {
    const event = realtimePublicServerEventSchema.parse({
      ...input,
      schemaVersion: 1,
      requestId: this.requestId,
      sequence: this.#sequence,
    });
    this.#sequence += 1;
    this.#log.push(event);
    this.#socket?.deliver(event);
  }
}

/** The fake's WebSocket, in the shape the SDK drives a browser one. */
export class FakeSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly url: string;
  readonly protocol = KAANA_REALTIME_SUBPROTOCOL;
  readyState = FakeSocket.CONNECTING;
  binaryType = 'arraybuffer';
  onopen: ((event: unknown) => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  readonly #server: FakeKaanaServer;
  #session: FakeRealtimeSession | undefined;

  constructor(server: FakeKaanaServer, url: string) {
    this.#server = server;
    this.url = url;
    queueMicrotask(() => {
      if (this.readyState !== FakeSocket.CONNECTING) return;
      this.readyState = FakeSocket.OPEN;
      this.onopen?.({});
    });
  }

  send(data: string): void {
    if (this.readyState !== FakeSocket.OPEN) throw new Error('the fake socket is not open');
    const frame = JSON.parse(data) as { type?: string };
    if (this.#session !== undefined) {
      this.#session.receive(frame);
      return;
    }
    this.#session = this.#server.attach(this, frame);
  }

  close(code = 1000): void {
    if (this.readyState === FakeSocket.CLOSED) return;
    this.readyState = FakeSocket.CLOSED;
    this.#session?.detach(this);
    queueMicrotask(() => this.onclose?.({ code }));
  }

  /** Delivered by the fake server, asynchronously as a network would. */
  deliver(event: RealtimeServerEvent | Record<string, unknown>): void {
    const data = JSON.stringify(event);
    queueMicrotask(() => {
      if (this.readyState === FakeSocket.OPEN) this.onmessage?.({ data });
    });
  }

  /** Closed from the server's side. */
  serverClose(code: number): void {
    if (this.readyState === FakeSocket.CLOSED) return;
    queueMicrotask(() => {
      if (this.readyState === FakeSocket.CLOSED) return;
      this.readyState = FakeSocket.CLOSED;
      this.onclose?.({ code });
    });
  }
}

/**
 * `https://kaana.ai/v1` in memory. Pass `fetch` and `WebSocket` to a `Kaana`
 * (`createFakeKaana` does), then read what it received and drive its sessions.
 */
export class FakeKaanaServer {
  readonly models: readonly Model[];
  /** Every HTTP request received, in order. */
  readonly requests: FakeRequest[] = [];
  /** Every session opened, in order. */
  readonly sessions: FakeRealtimeSession[] = [];
  readonly #transcribe: (upload: FakeTranscriptionUpload) => FakeTranscriptionResult;

  constructor(options: FakeKaanaServerOptions) {
    this.models = options.models.map((model) => modelSchema.parse(model));
    this.#transcribe = options.transcribe ?? (() => ({ text: '', durationMs: 1000 }));
  }

  /** The `fetch` a `Kaana` sends its HTTP requests through. */
  readonly fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const request = new Request(input, init);
    const url = new URL(request.url);
    const path = url.pathname.replace(/^\/v1/, '');
    this.requests.push({ method: request.method, path, headers: request.headers });
    const id = requestId();
    if (request.method === 'GET' && path === '/models') return json(200, { data: this.models }, id);
    if (request.method === 'GET' && path.startsWith('/models/')) {
      const model = this.find(decodeURIComponent(path.slice('/models/'.length)));
      return model === undefined
        ? json(404, publicError('model_not_found', 'No such model.', id, 'id'), id)
        : json(200, model, id);
    }
    if (request.method === 'POST' && path === '/audio/transcriptions') {
      return this.#transcription(request, id);
    }
    return json(
      404,
      publicError('invalid_request', `The fake does not serve ${request.method} ${path}.`, id),
      id,
    );
  };

  /** The `WebSocket` a `Kaana` opens its realtime sessions with. */
  get WebSocket(): typeof globalThis.WebSocket {
    const server = this;
    return class extends FakeSocket {
      constructor(url: string | URL) {
        super(server, String(url));
      }
    } as unknown as typeof globalThis.WebSocket;
  }

  /** The catalogue model a reference names, whatever revision it pins. */
  find(reference: string): Model | undefined {
    const id = reference.split('@')[0];
    return this.models.find((model) => model.modelId === id);
  }

  /** The first frame of a connection: an open, or a resume. */
  attach(socket: FakeSocket, frame: { type?: string }): FakeRealtimeSession | undefined {
    const id = requestId();
    const refuse = (code: PublicErrorCode, message: string, param?: string) => {
      socket.deliver({
        schemaVersion: 1,
        requestId: id,
        sequence: 0,
        type: 'error',
        fatal: true,
        error: publicError(code, message, id, param),
      });
      socket.serverClose(1008);
      return undefined;
    };
    if (frame.type === 'session.resume') {
      const resume = frame as { requestId?: string; afterSequence?: number };
      const session = this.sessions.find((candidate) => candidate.requestId === resume.requestId);
      if (session === undefined || session.closed) {
        return refuse('invalid_request', 'There is no session to resume.', 'requestId');
      }
      session.resume(socket, resume.afterSequence ?? -1);
      return session;
    }
    const model = this.find(new URL(socket.url).searchParams.get('model') ?? '');
    if (model === undefined) return refuse('model_not_found', 'No such model.', 'model');
    const open = realtimeSessionOpenSchema.safeParse(frame);
    if (!open.success) {
      return refuse('invalid_request', open.error.issues[0]?.message ?? 'invalid session.open');
    }
    // What Kaana checks before asking Oxy for a grant.
    const issue = checkRealtimeSessionOpen(open.data, model)[0];
    if (issue !== undefined) return refuse('invalid_request', issue.message, issue.param);
    const session = new FakeRealtimeSession(model, open.data, socket);
    this.sessions.push(session);
    return session;
  }

  async #transcription(request: Request, id: string): Promise<Response> {
    let fields: { model: string; language?: string; prompt?: string };
    let mediaType: string;
    let audio: Uint8Array;
    if ((request.headers.get('Content-Type') ?? '').startsWith('multipart/form-data')) {
      const form = await request.formData();
      const file = form.get('file');
      const parsed = transcriptionRequestSchema.safeParse(
        Object.fromEntries([...form.entries()].filter(([name]) => name !== 'file')),
      );
      if (!parsed.success || !(file instanceof Blob)) {
        return json(
          400,
          publicError('invalid_request', 'The upload is not a transcription request.', id, 'file'),
          id,
        );
      }
      fields = parsed.data;
      mediaType = file.type;
      audio = new Uint8Array(await file.arrayBuffer());
    } else {
      const parsed = transcriptionJsonRequestSchema.safeParse(await request.json());
      if (!parsed.success) {
        const issue = parsed.error.issues[0];
        return json(
          400,
          publicError('invalid_request', issue?.message ?? 'invalid', id, issue?.path.join('.')),
          id,
        );
      }
      fields = parsed.data;
      mediaType = parsed.data.audio.mediaType;
      audio = decodeBase64(parsed.data.audio.data);
    }
    const model = this.find(fields.model);
    if (model === undefined)
      return json(404, publicError('model_not_found', 'No such model.', id, 'model'), id);
    const issue =
      model.audio === undefined
        ? undefined
        : checkTranscriptionRequest(mediaType as never, model.audio)[0];
    if (issue !== undefined)
      return json(400, publicError('invalid_request', issue.message, id, issue.param), id);
    const result = this.#transcribe({
      model,
      mediaType,
      audio,
      ...(fields.language === undefined ? {} : { language: fields.language }),
      ...(fields.prompt === undefined ? {} : { prompt: fields.prompt }),
    });
    const transcription: Transcription = {
      schemaVersion: 1,
      requestId: id,
      model: `${model.modelId}@${model.currentRevision}` as ModelReference,
      text: result.text,
      ...(result.language === undefined ? {} : { language: result.language }),
      durationMs: result.durationMs,
      usage: [{ unit: 'audio_input_milliseconds', quantity: result.durationMs }],
    };
    return json(200, transcription, id);
  }
}
