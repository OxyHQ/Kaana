/**
 * The one place a request is sent: credentials, headers, the deadline, the
 * caller's abort signal, typed error bodies and retries before the first byte.
 */

import {
  chatCompletionErrorSchema,
  INFERENCE_CONTRACT_VERSION,
  KAANA_API_BASE_URL,
  KAANA_CONTRACT_VERSION_HEADER,
  KAANA_IDEMPOTENCY_KEY_HEADER,
  KAANA_ON_BEHALF_OF_HEADER,
  KAANA_REQUEST_ID_HEADER,
  publicErrorSchema,
} from '@kaana.ai/contracts';
import {
  cancelledByCaller,
  fromPublicError,
  isRetryableCode,
  KaanaAuthenticationError,
  KaanaConnectionError,
  type KaanaError,
  kaanaError,
  KaanaProtocolError,
  KaanaUnavailableError,
} from './errors.js';

/**
 * The value of `Authorization: Bearer`. A function is called on every request
 * (every attempt); the SDK never caches it: whoever supplies the function
 * already caches and renews.
 */
export type KaanaCredential = string | (() => string | Promise<string>);

/**
 * The fetch the SDK calls. `globalThis.fetch` (or any WHATWG-compatible fetch)
 * satisfies it; it is written structurally so a runtime's extra members are
 * not required of a test double.
 */
export type KaanaFetch = (input: string, init: RequestInit) => Promise<Response>;

export interface KaanaOptions {
  credential: KaanaCredential;
  /** Only `https://kaana.ai/v1`, or a loopback origin in development. */
  baseURL?: string;
  fetch?: KaanaFetch;
  /** For realtime where there is no global WebSocket (e.g. `ws` in Node < 22). */
  WebSocket?: typeof globalThis.WebSocket;
  /** Default 120 000. */
  timeoutMs?: number;
  /** Default 2: only before the first byte and only on retryable codes. */
  maxRetries?: number;
}

/** Shared workload transport; its owner controls identity and renewal. */
export interface KaanaAuthenticatedTransport {
  fetch: KaanaFetch;
  getAccessToken(target: string | URL, options?: { signal?: AbortSignal }): Promise<string>;
}
export interface KaanaTransportOptions extends Omit<KaanaOptions, 'credential' | 'fetch'> {
  transport: KaanaAuthenticatedTransport;
  credential?: never;
  fetch?: never;
  /** Fixed inference attribution; never authority to read private account data. */
  accountId?: string;
}

export interface RequestOptions {
  signal?: AbortSignal;
  /** `Idempotency-Key`; reused unchanged by every retry. */
  idempotencyKey?: string;
  /** Delegated attribution (`X-Oxy-User-Id`); honoured only with a service token. */
  onBehalfOfUserId?: string;
  timeoutMs?: number;
}

/**
 * A realtime session's options: a request's, without `idempotencyKey`. A
 * session is opened once over one WebSocket and resumed by its own
 * `session.resume`, so there is no retried open for a key to deduplicate, and
 * a key that changed nothing is not offered (SDK 2.0.0).
 */
export type RealtimeConnectOptions = Omit<RequestOptions, 'idempotencyKey'>;

export type ErrorDialect = 'public' | 'chat';

export interface SendInit {
  method: 'GET' | 'POST';
  path: string;
  query?: Record<string, string>;
  /** A JSON string or a multipart form. */
  body?: string | FormData;
  accept: string;
  dialect?: ErrorDialect;
  options?: RequestOptions;
  /**
   * A streamed response: the deadline covers only the wait for the response
   * headers, never the stream itself.
   */
  stream?: boolean;
}

/** An accepted (2xx) response and the handles that govern its body. */
export interface Sent {
  readonly response: Response;
  readonly requestId: string;
  /** Aborts when the deadline passes or the caller aborts. */
  readonly signal: AbortSignal;
  /** True once the deadline has passed. */
  timedOut(): boolean;
  /** True once the caller's signal (or `abort()`) fired. */
  aborted(): boolean;
  /** Abort the body read. */
  abort(reason?: unknown): void;
  /** Clear the deadline and detach from the caller's signal. */
  release(): void;
}

const DEFAULT_TIMEOUT_MS = 120_000;
const DEFAULT_MAX_RETRIES = 2;
const BASE_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 8_000;
const MAX_ERROR_BODY_CHARACTERS = 64 * 1024;
const LOOPBACK_HOSTS = new Set(['localhost', '127.0.0.1', '[::1]']);

export class Transport {
  readonly baseURL: string;
  readonly WebSocket: typeof globalThis.WebSocket | undefined;
  readonly timeoutMs: number;
  readonly #credential: KaanaCredential | undefined;
  readonly #authenticated: KaanaAuthenticatedTransport | undefined;
  readonly #accountId: string | undefined;
  readonly #fetch: KaanaFetch;
  readonly #maxRetries: number;

  constructor(options: KaanaOptions | KaanaTransportOptions) {
    if (options === undefined || options === null)
      throw new TypeError('Kaana: options.credential is required');
    const authenticated = 'transport' in options;
    if (authenticated) {
      if (
        options.credential !== undefined ||
        options.fetch !== undefined ||
        typeof options.transport?.fetch !== 'function' ||
        typeof options.transport?.getAccessToken !== 'function'
      ) {
        throw new TypeError(
          'Kaana: authenticated transport cannot be combined with a credential or fetch override',
        );
      }
      this.#authenticated = options.transport;
      this.#accountId = options.accountId;
    } else {
      if (options.credential === undefined)
        throw new TypeError('Kaana: options.credential is required');
      if (typeof options.credential !== 'string' && typeof options.credential !== 'function') {
        throw new TypeError('Kaana: options.credential is a string or a function returning one');
      }
      this.#credential = options.credential;
    }
    this.baseURL = normalizeBaseURL(options.baseURL ?? KAANA_API_BASE_URL);
    const fetchImpl: KaanaFetch | undefined = authenticated
      ? options.transport.fetch
      : (options.fetch ?? globalThis.fetch);
    if (typeof fetchImpl !== 'function')
      throw new TypeError('Kaana: no fetch is available; pass options.fetch');
    this.#fetch = (input, init) => fetchImpl(input, init);
    this.WebSocket = options.WebSocket ?? globalThis.WebSocket;
    this.timeoutMs = positiveInteger(options.timeoutMs, DEFAULT_TIMEOUT_MS, 'timeoutMs');
    const retries = options.maxRetries ?? DEFAULT_MAX_RETRIES;
    if (!Number.isInteger(retries) || retries < 0) {
      throw new TypeError('Kaana: options.maxRetries is a non-negative integer');
    }
    this.#maxRetries = retries;
  }

  /** The credential, read now. Never stored. */
  async credential(target?: string, signal?: AbortSignal): Promise<string> {
    if (this.#authenticated) {
      if (!target) throw new TypeError('Kaana: the credential destination is required');
      return this.#authenticated.getAccessToken(target, { signal });
    }
    const value =
      typeof this.#credential === 'function' ? await this.#credential() : this.#credential;
    if (typeof value !== 'string' || value.length === 0) {
      throw new KaanaAuthenticationError({
        code: 'authentication_failed',
        message:
          'The credential is empty. Create an API key in Oxy Console (https://console.oxy.so).',
        status: null,
        requestId: null,
        retryable: false,
      });
    }
    return value;
  }

  attribution(requested?: string): string | undefined {
    if (!this.#authenticated) return requested;
    if (requested !== undefined && requested !== this.#accountId)
      throw new TypeError('Kaana: requester attribution differs from this client');
    return this.#accountId;
  }

  url(path: string, query?: Record<string, string>): string {
    const search = query === undefined ? '' : `?${new URLSearchParams(query).toString()}`;
    return `${this.baseURL}${path}${search}`;
  }

  /** Send with retries before the first byte. Resolves only with a 2xx. */
  async send(init: SendInit): Promise<Sent> {
    const options = init.options ?? {};
    const timeoutMs = positiveInteger(options.timeoutMs, this.timeoutMs, 'timeoutMs');
    for (let attempt = 0; ; attempt += 1) {
      if (options.signal?.aborted) throw cancelledByCaller(null);
      const attributed = this.attribution(options.onBehalfOfUserId);
      const headers: Record<string, string> = {
        ...(this.#authenticated ? {} : { Authorization: `Bearer ${await this.credential()}` }),
        Accept: init.accept,
        // The set this SDK parses: Kaana renders what it sends in that shape.
        [KAANA_CONTRACT_VERSION_HEADER]: INFERENCE_CONTRACT_VERSION,
      };
      if (typeof init.body === 'string') headers['Content-Type'] = 'application/json';
      if (options.idempotencyKey !== undefined) {
        headers[KAANA_IDEMPOTENCY_KEY_HEADER] = options.idempotencyKey;
      }
      if (attributed !== undefined) headers[KAANA_ON_BEHALF_OF_HEADER] = attributed;

      const control = attemptControl(options.signal, timeoutMs);
      let response: Response;
      try {
        response = await this.#fetch(this.url(init.path, init.query), {
          method: init.method,
          ...(this.#authenticated ? { redirect: 'error' as const } : {}),
          headers,
          ...(init.body === undefined ? {} : { body: init.body }),
          signal: control.signal,
        });
      } catch (cause) {
        control.release();
        if (control.aborted()) throw cancelledByCaller(null);
        if (control.timedOut()) {
          throw new KaanaConnectionError(`No response within ${timeoutMs} ms.`, {
            cause,
            timedOut: true,
          });
        }
        const failure = new KaanaConnectionError('The request could not reach Kaana.', { cause });
        if (attempt < this.#maxRetries) {
          await wait(backoff(attempt), options.signal);
          continue;
        }
        throw failure;
      }

      if (!response.ok) {
        let failure: KaanaError | KaanaProtocolError;
        try {
          failure = await errorFromResponse(response, init.dialect ?? 'public');
        } catch (cause) {
          control.release();
          if (control.aborted()) throw cancelledByCaller(null);
          throw new KaanaConnectionError('The error response could not be read.', {
            cause,
            timedOut: control.timedOut(),
          });
        }
        control.release();
        if (
          failure instanceof KaanaProtocolError ||
          !failure.retryable ||
          attempt >= this.#maxRetries
        ) {
          throw failure;
        }
        await wait(failure.retryAfterMs ?? backoff(attempt), options.signal);
        continue;
      }

      const requestId = response.headers.get(KAANA_REQUEST_ID_HEADER);
      if (requestId === null || requestId.length === 0) {
        control.abort();
        control.release();
        await response.body?.cancel().catch(() => undefined);
        throw new KaanaProtocolError(`The response carries no ${KAANA_REQUEST_ID_HEADER}.`, null);
      }
      if (init.stream) control.clearDeadline();
      return { response, requestId, ...control };
    }
  }

  /** Send, read the whole JSON body and validate it. */
  async json<T>(
    init: SendInit,
    schema: { safeParse(value: unknown): SafeParse<T> },
  ): Promise<{ value: T; requestId: string; response: Response }> {
    const sent = await this.send(init);
    let text: string;
    try {
      text = await sent.response.text();
    } catch (cause) {
      throw bodyReadFailure(sent, cause);
    } finally {
      sent.release();
    }
    return {
      value: parseJson(text, schema, sent.requestId),
      requestId: sent.requestId,
      response: sent.response,
    };
  }
}

export type SafeParse<T> =
  | { success: true; data: T }
  | { success: false; error: { issues: readonly { path: readonly PropertyKey[] }[] } };

/** Parse and validate a JSON document, or fail as a protocol error. */
export function parseJson<T>(
  text: string,
  schema: { safeParse(value: unknown): SafeParse<T> },
  requestId: string | null,
): T {
  let payload: unknown;
  try {
    payload = JSON.parse(text);
  } catch (_error) {
    throw new KaanaProtocolError('The response is not JSON.', requestId);
  }
  return validate(payload, schema, requestId, 'response');
}

export function validate<T>(
  payload: unknown,
  schema: { safeParse(value: unknown): SafeParse<T> },
  requestId: string | null,
  what: string,
): T {
  const parsed = schema.safeParse(payload);
  if (!parsed.success) {
    const path = parsed.error.issues[0]?.path ?? [];
    throw new KaanaProtocolError(
      `The ${what} does not satisfy @kaana.ai/contracts at ${path.length === 0 ? 'its root' : path.map(String).join('.')}.`,
      requestId,
    );
  }
  return parsed.data;
}

/** A body read that failed: the caller's abort, the deadline, or the network. */
export function bodyReadFailure(sent: Sent, cause: unknown): Error {
  if (sent.aborted()) return cancelledByCaller(sent.requestId);
  return new KaanaConnectionError(
    sent.timedOut() ? 'The response did not complete in time.' : 'The response was cut off.',
    { cause, timedOut: sent.timedOut() },
  );
}

/** The `Content-Type` essence: lowercase, without parameters. */
export function mediaTypeOf(response: Response): string | undefined {
  return response.headers.get('Content-Type')?.split(';', 1)[0]?.trim().toLowerCase();
}

/* -------------------------------------------------------------------------- */

function normalizeBaseURL(raw: string): string {
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch (_error) {
    throw new TypeError(`Kaana: baseURL ${JSON.stringify(raw)} is not a URL`);
  }
  const normalized = `${parsed.origin}${parsed.pathname.replace(/\/+$/, '')}`;
  if (normalized === KAANA_API_BASE_URL) return normalized;
  const loopback =
    (parsed.protocol === 'http:' || parsed.protocol === 'https:') &&
    LOOPBACK_HOSTS.has(parsed.hostname);
  if (!loopback || parsed.search !== '' || parsed.hash !== '' || parsed.username !== '') {
    throw new TypeError(
      `Kaana: baseURL must be ${KAANA_API_BASE_URL} or a loopback origin in development`,
    );
  }
  return normalized;
}

function positiveInteger(value: number | undefined, fallback: number, name: string): number {
  if (value === undefined) return fallback;
  if (!Number.isFinite(value) || value <= 0) {
    throw new TypeError(`Kaana: ${name} is a positive number of milliseconds`);
  }
  return value;
}

interface AttemptControl {
  readonly signal: AbortSignal;
  timedOut(): boolean;
  aborted(): boolean;
  abort(reason?: unknown): void;
  clearDeadline(): void;
  release(): void;
}

function attemptControl(caller: AbortSignal | undefined, timeoutMs: number): AttemptControl {
  const controller = new AbortController();
  let timedOut = false;
  let aborted = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort(new Error('timeout'));
  }, timeoutMs);
  const onAbort = () => {
    aborted = true;
    controller.abort(caller?.reason);
  };
  caller?.addEventListener('abort', onAbort, { once: true });
  return {
    signal: controller.signal,
    timedOut: () => timedOut,
    aborted: () => aborted,
    abort: (reason?: unknown) => {
      aborted = true;
      controller.abort(reason);
    },
    clearDeadline: () => clearTimeout(timer),
    release: () => {
      clearTimeout(timer);
      caller?.removeEventListener('abort', onAbort);
    },
  };
}

async function errorFromResponse(
  response: Response,
  dialect: ErrorDialect,
): Promise<KaanaError | KaanaProtocolError> {
  const status = response.status;
  const headerRequestId = response.headers.get(KAANA_REQUEST_ID_HEADER);
  const retryAfterHeader = retryAfterMs(response.headers.get('Retry-After'));
  const text = (await response.text()).slice(0, MAX_ERROR_BODY_CHARACTERS);
  let payload: unknown;
  try {
    payload = JSON.parse(text);
  } catch (_error) {
    payload = undefined;
  }

  if (dialect === 'chat') {
    const parsed = chatCompletionErrorSchema.safeParse(payload);
    if (parsed.success) {
      const { code, message, param } = parsed.data.error;
      const retryable = isRetryableCode(code);
      return kaanaError({
        code,
        message,
        status,
        requestId: headerRequestId,
        retryable,
        ...(retryable && retryAfterHeader !== undefined ? { retryAfterMs: retryAfterHeader } : {}),
        ...(param === null ? {} : { param }),
      });
    }
  } else {
    const parsed = publicErrorSchema.safeParse(payload);
    if (parsed.success) {
      const error = fromPublicError(parsed.data, status);
      if (error.retryable && error.retryAfterMs === undefined && retryAfterHeader !== undefined) {
        return kaanaError({
          code: error.code,
          message: error.message,
          status,
          requestId: error.requestId,
          retryable: true,
          retryAfterMs: retryAfterHeader,
          ...(error.param === undefined ? {} : { param: error.param }),
        });
      }
      return error;
    }
  }

  // A gateway in front of Kaana (or a load balancer with no healthy target)
  // answers without the contract's body. That is an outage, not a protocol
  // violation by Kaana, and it is as retryable as Kaana's own 503.
  if (status === 502 || status === 503 || status === 504) {
    return new KaanaUnavailableError({
      code: 'service_unavailable',
      message: `Kaana is unavailable (HTTP ${status}).`,
      status,
      requestId: headerRequestId,
      retryable: true,
      ...(retryAfterHeader === undefined ? {} : { retryAfterMs: retryAfterHeader }),
    });
  }
  return new KaanaProtocolError(
    `HTTP ${status} carries no error body of @kaana.ai/contracts.`,
    headerRequestId,
  );
}

function retryAfterMs(header: string | null): number | undefined {
  if (header === null) return undefined;
  const trimmed = header.trim();
  if (/^\d+$/.test(trimmed)) return Number(trimmed) * 1000;
  const at = Date.parse(trimmed);
  if (Number.isNaN(at)) return undefined;
  return Math.max(0, at - Date.now());
}

function backoff(attempt: number): number {
  const ceiling = Math.min(MAX_BACKOFF_MS, BASE_BACKOFF_MS * 2 ** attempt);
  return Math.round(ceiling / 2 + Math.random() * (ceiling / 2));
}

function wait(milliseconds: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(cancelledByCaller(null));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(cancelledByCaller(null));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, milliseconds);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}
