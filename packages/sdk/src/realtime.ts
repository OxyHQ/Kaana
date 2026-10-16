/** `GET /v1/realtime`: one WebSocket session, resumable, settled once on close. */

import {
  KAANA_REALTIME_BEARER_PROTOCOL_PREFIX,
  KAANA_REALTIME_SUBPROTOCOL,
  KAANA_REALTIME_USER_PROTOCOL_PREFIX,
  type ModelReference,
  modelReferenceSchema,
  type RealtimeClientCommand,
  type RealtimeServerEvent,
  type RealtimeSessionOpen,
  realtimeClientCommandSchema,
  realtimePublicServerEventSchema,
  realtimeSessionOpenSchema,
  type Usage,
} from '@kaana.ai/contracts';
import { encodeBase64, encodeBase64Url, utf8 } from './base64.js';
import {
  cancelledByCaller,
  clientRefusal,
  fromPublicError,
  type KaanaError,
  KaanaConnectionError,
  KaanaProtocolError,
} from './errors.js';
import type { RealtimeConnectOptions, Transport } from './http.js';
import { deferred } from './stream.js';

/** What `realtime.connect` opens: the `session.open` frame and the model it is for. */
export type RealtimeConnectRequest = RealtimeSessionOpen & { model: ModelReference };

type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never;

/**
 * A client command. `schemaVersion`, `requestId` (the session's) and
 * `commandId` (a fresh unique id) are filled in when omitted.
 */
export type RealtimeClientEventInput = DistributiveOmit<
  RealtimeClientCommand,
  'schemaVersion' | 'requestId' | 'commandId'
> &
  Partial<Pick<RealtimeClientCommand, 'schemaVersion' | 'requestId' | 'commandId'>>;

export interface RealtimeSession extends AsyncIterable<RealtimeServerEvent> {
  /** The session's request id, from `session.created`. */
  readonly sessionId: string;
  send(event: RealtimeClientEventInput): void;
  /** Raw PCM16 in, as `input_audio.append` frames of at most 49 152 bytes. */
  appendAudio(pcm16: Uint8Array): void;
  /** Reattach after a dropped connection, within the session's resume window. */
  resume(): Promise<void>;
  /** End the session; resolves with what it consumed. It settles exactly once. */
  close(): Promise<Usage>;
}

/** The most audio one `input_audio.append` frame carries. */
export const MAX_REALTIME_AUDIO_FRAME_BYTES = 49_152;

type QueueEntry =
  | { kind: 'event'; event: RealtimeServerEvent }
  | { kind: 'error'; error: unknown }
  | { kind: 'end' };

class EventQueue {
  readonly #entries: QueueEntry[] = [];
  #waiters: ((entry: QueueEntry) => void)[] = [];
  #ended = false;

  push(entry: QueueEntry): void {
    if (this.#ended) return;
    if (entry.kind === 'end') this.#ended = true;
    const waiter = this.#waiters.shift();
    if (waiter !== undefined) waiter(entry);
    else this.#entries.push(entry);
    if (this.#ended) {
      for (const pending of this.#waiters.splice(0)) pending({ kind: 'end' });
    }
  }

  take(): Promise<QueueEntry> {
    const entry = this.#entries.shift();
    if (entry !== undefined) return Promise.resolve(entry);
    if (this.#ended) return Promise.resolve({ kind: 'end' });
    return new Promise((resolve) => this.#waiters.push(resolve));
  }
}

type State = 'opening' | 'open' | 'resuming' | 'dropped' | 'closed';

export async function connectRealtime(
  transport: Transport,
  request: RealtimeConnectRequest,
  options: RealtimeConnectOptions,
): Promise<RealtimeSession> {
  const { model, ...open } = request;
  if (!modelReferenceSchema.safeParse(model).success) {
    throw clientRefusal(
      'model',
      'realtime.connect needs a model reference publisher/model[@revision]',
    );
  }
  const parsed = realtimeSessionOpenSchema.safeParse(open);
  if (!parsed.success) {
    const issue = parsed.error.issues[0];
    throw clientRefusal(
      issue?.path.map(String).join('.') || 'session',
      issue?.message ?? 'invalid',
    );
  }
  const WebSocketImpl = transport.WebSocket;
  if (typeof WebSocketImpl !== 'function') {
    throw new TypeError('Kaana: no WebSocket is available; pass options.WebSocket');
  }
  const credential = await transport.credential();
  const protocols = [
    KAANA_REALTIME_SUBPROTOCOL,
    `${KAANA_REALTIME_BEARER_PROTOCOL_PREFIX}${encodeBase64Url(utf8(credential))}`,
  ];
  if (options.onBehalfOfUserId !== undefined) {
    protocols.push(
      `${KAANA_REALTIME_USER_PROTOCOL_PREFIX}${encodeBase64Url(utf8(options.onBehalfOfUserId))}`,
    );
  }
  const url = `${transport.baseURL.replace(/^http/, 'ws')}/realtime?${new URLSearchParams({ model })}`;
  const session = new Session(
    WebSocketImpl,
    url,
    protocols,
    options.timeoutMs ?? transport.timeoutMs,
  );
  await session.open(JSON.stringify(open), options.signal);
  return session;
}

class Session implements RealtimeSession {
  readonly #WebSocket: typeof globalThis.WebSocket;
  readonly #url: string;
  readonly #protocols: string[];
  readonly #timeoutMs: number;
  readonly #queue = new EventQueue();
  #ws: WebSocket | undefined;
  #state: State = 'opening';
  #sessionId = '';
  #lastSequence = -1;
  #fatal: KaanaError | undefined;
  #units: Usage | undefined;
  #commands = 0;
  #pending: { resolve(): void; reject(error: unknown): void } | undefined;
  #closing = deferred<Usage>();

  constructor(
    WebSocketImpl: typeof globalThis.WebSocket,
    url: string,
    protocols: string[],
    timeoutMs: number,
  ) {
    this.#WebSocket = WebSocketImpl;
    this.#url = url;
    this.#protocols = protocols;
    this.#timeoutMs = timeoutMs;
  }

  get sessionId(): string {
    return this.#sessionId;
  }

  open(frame: string, signal: AbortSignal | undefined): Promise<void> {
    return this.#attach(frame, 'opening', signal);
  }

  resume(): Promise<void> {
    if (this.#state === 'closed') {
      return Promise.reject(new KaanaConnectionError('The session has ended.'));
    }
    if (this.#state === 'resuming' || this.#state === 'opening') {
      return Promise.reject(new KaanaConnectionError('The session is already reattaching.'));
    }
    const frame = JSON.stringify({
      schemaVersion: 1,
      requestId: this.#sessionId,
      commandId: this.#nextCommandId(),
      type: 'session.resume',
      afterSequence: this.#lastSequence,
    });
    const previous = this.#ws;
    this.#ws = undefined;
    previous?.close(1000);
    return this.#attach(frame, 'resuming', undefined);
  }

  send(event: RealtimeClientEventInput): void {
    if (this.#state !== 'open' || this.#ws === undefined) {
      throw new KaanaConnectionError(
        this.#state === 'closed'
          ? 'The session has ended.'
          : 'The session is not attached; call resume().',
      );
    }
    const command = {
      ...event,
      schemaVersion: event.schemaVersion ?? 1,
      requestId: event.requestId ?? this.#sessionId,
      commandId: event.commandId ?? this.#nextCommandId(),
    };
    const parsed = realtimeClientCommandSchema.safeParse(command);
    if (!parsed.success) {
      const issue = parsed.error.issues[0];
      throw clientRefusal(
        issue?.path.map(String).join('.') || 'event',
        issue?.message ?? 'invalid',
      );
    }
    this.#ws.send(JSON.stringify(command));
  }

  appendAudio(pcm16: Uint8Array): void {
    for (let offset = 0; offset < pcm16.byteLength; offset += MAX_REALTIME_AUDIO_FRAME_BYTES) {
      const chunk = pcm16.subarray(offset, offset + MAX_REALTIME_AUDIO_FRAME_BYTES);
      this.send({ type: 'input_audio.append', data: encodeBase64(chunk) });
    }
  }

  close(): Promise<Usage> {
    if (this.#units !== undefined) return Promise.resolve(this.#units);
    if (this.#state === 'closed') {
      return Promise.reject(
        this.#fatal ?? new KaanaConnectionError('The session ended without settling here.'),
      );
    }
    this.send({ type: 'session.close' });
    const timer = setTimeout(() => {
      this.#closing.reject(
        new KaanaConnectionError('The session did not close in time.', { timedOut: true }),
      );
    }, this.#timeoutMs);
    return this.#closing.promise.finally(() => clearTimeout(timer));
  }

  async *[Symbol.asyncIterator](): AsyncIterator<RealtimeServerEvent> {
    while (true) {
      const entry = await this.#queue.take();
      if (entry.kind === 'end') return;
      if (entry.kind === 'error') throw entry.error;
      yield entry.event;
    }
  }

  /* ------------------------------------------------------------------------ */

  #nextCommandId(): string {
    this.#commands += 1;
    const random =
      typeof globalThis.crypto?.randomUUID === 'function'
        ? globalThis.crypto.randomUUID()
        : `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
    return `cmd_${this.#commands}_${random}`;
  }

  #attach(frame: string, state: 'opening' | 'resuming', signal: AbortSignal | undefined) {
    return new Promise<void>((resolve, reject) => {
      if (signal?.aborted) {
        reject(cancelledByCaller(null));
        return;
      }
      this.#state = state;
      let ws: WebSocket;
      try {
        ws = new this.#WebSocket(this.#url, this.#protocols);
      } catch (cause) {
        if (state === 'resuming') this.#state = 'dropped';
        reject(new KaanaConnectionError('The realtime connection could not be opened.', { cause }));
        return;
      }
      this.#ws = ws;
      const settle = (error?: unknown) => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', onAbort);
        this.#pending = undefined;
        if (error === undefined) resolve();
        else reject(error);
      };
      const timer = setTimeout(() => {
        this.#fail(
          ws,
          new KaanaConnectionError('The realtime session did not open in time.', {
            timedOut: true,
          }),
        );
      }, this.#timeoutMs);
      const onAbort = () => this.#fail(ws, cancelledByCaller(this.#sessionId || null));
      signal?.addEventListener('abort', onAbort, { once: true });
      this.#pending = { resolve: () => settle(), reject: (error) => settle(error) };

      ws.onopen = () => {
        if (ws !== this.#ws) return;
        if (
          typeof ws.protocol === 'string' &&
          ws.protocol.length > 0 &&
          ws.protocol !== KAANA_REALTIME_SUBPROTOCOL
        ) {
          this.#fail(ws, new KaanaProtocolError(`The server selected ${ws.protocol}.`, null));
          return;
        }
        ws.send(frame);
      };
      ws.onmessage = (message: MessageEvent) => this.#receive(ws, message.data);
      ws.onclose = () => this.#closed(ws);
      ws.onerror = () => {
        // A close always follows; it decides what the failure means.
      };
    });
  }

  /** Fail the pending open/resume (if any) and drop the connection. */
  #fail(ws: WebSocket, error: unknown): void {
    if (ws !== this.#ws) return;
    const pending = this.#pending;
    if (pending !== undefined) {
      this.#state = this.#state === 'opening' ? 'closed' : 'dropped';
      this.#ws = undefined;
      ws.close(1000);
      pending.reject(error);
      return;
    }
    this.#ws = undefined;
    ws.close(1000);
    this.#drop(error);
  }

  #receive(ws: WebSocket, data: unknown): void {
    if (ws !== this.#ws) return;
    if (typeof data !== 'string') {
      this.#fail(
        ws,
        new KaanaProtocolError('The server sent a binary frame.', this.#sessionId || null),
      );
      return;
    }
    let payload: unknown;
    try {
      payload = JSON.parse(data);
    } catch (_error) {
      this.#fail(ws, new KaanaProtocolError('The server sent a frame that is not JSON.', null));
      return;
    }
    const parsed = realtimePublicServerEventSchema.safeParse(payload);
    if (!parsed.success) {
      const path = parsed.error.issues[0]?.path.map(String).join('.') || 'its root';
      this.#fail(
        ws,
        new KaanaProtocolError(
          `The server sent an event outside @kaana.ai/contracts at ${path}.`,
          this.#sessionId || null,
        ),
      );
      return;
    }
    const event = parsed.data;

    if (this.#state === 'opening') {
      if (event.type === 'session.created') {
        this.#sessionId = event.requestId;
        this.#lastSequence = event.sequence;
        this.#state = 'open';
        this.#queue.push({ kind: 'event', event });
        this.#pending?.resolve();
      } else if (event.type === 'error') {
        this.#fail(ws, fromPublicError(event.error, null));
      } else {
        this.#fail(ws, new KaanaProtocolError(`The session began with ${event.type}.`, null));
      }
      return;
    }

    if (event.sequence <= this.#lastSequence) {
      if (this.#state === 'resuming' && event.type === 'error') {
        // The refusal of a resume stands outside the session's sequence.
        this.#fail(ws, fromPublicError(event.error, null));
      }
      return; // Already delivered before the resume.
    }
    this.#lastSequence = event.sequence;
    if (event.type === 'error' && event.fatal) {
      this.#fatal = fromPublicError(event.error, null, true);
    }
    this.#queue.push({ kind: 'event', event });
    if (event.type === 'session.resumed' && this.#state === 'resuming') {
      this.#state = 'open';
      this.#pending?.resolve();
    }
    if (event.type === 'session.closed') {
      this.#units = event.units;
      this.#state = 'closed';
      this.#queue.push({ kind: 'end' });
      this.#closing.resolve(event.units);
    }
  }

  #closed(ws: WebSocket): void {
    if (ws !== this.#ws) return;
    this.#ws = undefined;
    if (this.#pending !== undefined) {
      const opening = this.#state === 'opening';
      this.#state = opening ? 'closed' : 'dropped';
      this.#pending.reject(
        new KaanaConnectionError(
          opening
            ? 'The connection closed before the session opened.'
            : 'The connection closed before the session resumed.',
        ),
      );
      return;
    }
    if (this.#state === 'closed') return;
    this.#drop(
      new KaanaConnectionError(
        'The realtime connection dropped; call resume() within the resume window.',
      ),
    );
  }

  #drop(error: unknown): void {
    if (this.#fatal !== undefined) {
      // The server ended the session with a fatal error and no session.closed.
      this.#state = 'closed';
      this.#queue.push({ kind: 'error', error: this.#fatal });
      this.#queue.push({ kind: 'end' });
      this.#closing.reject(this.#fatal);
      return;
    }
    this.#state = 'dropped';
    this.#queue.push({ kind: 'error', error });
  }
}
