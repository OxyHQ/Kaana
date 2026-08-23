/**
 * The Kaana client: sign an envelope, post it, read the normalized event stream.
 *
 * ## One response shape, always
 *
 * `internal/httpapi` answers `POST /internal/v1/inference` with 200 and an event
 * stream, or with a non-200 and a contract error body. Nothing in between: a
 * status code answers exactly one question — was this a well-formed envelope
 * from the Oxy edge — and once the answer is yes, every outcome INCLUDING a
 * refusal arrives as the stream's terminal error event. The alternative would
 * have to choose a status before the first byte of a stream that has not been
 * produced yet, and a request that fails after two hundred tokens has already
 * sent 200.
 *
 * The envelope's own `stream` flag governs the UPSTREAM hop — whether Kaana
 * streams from the provider. It does not change how Kaana answers, which is why
 * {@link KaanaClient.complete} drains an event stream rather than reading a JSON
 * body.
 *
 * ## What this client does not do
 *
 * No retries, no fallback, no route selection, and no cost. Retryability is the
 * contract's producer assertion on the error body; routing execution and
 * failover are the data plane's; the customer's amount is Oxy's. A client
 * re-deriving any of them would be a second, staler copy of something that
 * already has one owner.
 */

import {
  inferenceErrorSchema,
  type InferenceFinishReason,
  type InferenceRequest,
  type InferenceStreamRouteSwitchEvent,
  type InferenceStreamStartEvent,
  type NormalizedUsageReport,
  type UsageQuantity,
  type UsageSource,
} from '@oxyhq/contracts';

import { serializeInferenceRequest } from './envelope.js';
import { KaanaInferenceError, KaanaProtocolError, KaanaTransportError } from './errors.js';
import { kaanaHealthSchema, type KaanaHealth } from './health.js';
import { signEdgeRequest, type KaanaSigningKey } from './signing.js';
import { readSseFrames } from './sse.js';
import {
  decodeKaanaFrame,
  KAANA_FRAME_STREAM_EVENT,
  KAANA_FRAME_USAGE_REPORT,
  type KaanaFrame,
} from './stream.js';

/** Kaana's two signed routes. `GET /livez` is unsigned and carries no detail. */
export const KAANA_INFERENCE_PATH = '/internal/v1/inference';
export const KAANA_HEALTH_PATH = '/internal/v1/health';

/** Echoes the envelope's request id, so a failure is correlatable. */
export const KAANA_REQUEST_ID_HEADER = 'X-Oxy-Request-Id';

export interface KaanaClientOptions {
  /** Kaana's origin, e.g. `https://relay.oxy.so`. */
  readonly baseUrl: string;
  readonly signingKey: KaanaSigningKey;
  /**
   * Injected so a test can drive the transport without a network, and so a
   * caller can supply an agent-bound fetch. Defaults to the global.
   */
  readonly fetch?: typeof globalThis.fetch;
  /**
   * The clock the signature is stamped with. Defaults to `Date.now`.
   *
   * Kaana refuses a signature outside `EDGE_SIGNATURE_MAX_SKEW_MS` in either
   * direction, so this is where a skewed host surfaces.
   */
  readonly now?: () => number;
}

export interface KaanaCallOptions {
  /**
   * Cancels the request. `internal/httpapi` hands the request context straight
   * to the adapter's upstream call, so aborting here really does stop the
   * upstream work rather than only stopping the reading of it.
   */
  readonly signal?: AbortSignal;
}

/** One tool call, accumulated across the `tool_call` events that built it. */
export interface KaanaToolCall {
  readonly toolCallId: string;
  readonly name: string;
  /**
   * The JSON TEXT the model emitted, unparsed. Models emit invalid JSON often
   * enough that parsing here would turn a recoverable model mistake into a
   * thrown error; `complete` says when the accumulated text is worth parsing.
   */
  readonly arguments: string;
  readonly complete: boolean;
}

/** What a drained stream amounts to. */
export interface KaanaCompletion {
  readonly requestId: string;
  readonly generationId?: string;
  /** What was actually resolved and served. */
  readonly start: InferenceStreamStartEvent;
  /**
   * Visible output. Never merged with the others: a client that renders
   * reasoning as answer text is a product bug, not a preference, which is why
   * the contract separates the channels in the first place.
   */
  readonly text: string;
  readonly reasoning: string;
  readonly refusal: string;
  readonly toolCalls: readonly KaanaToolCall[];
  readonly finishReason: InferenceFinishReason;
  readonly routeSwitches: readonly InferenceStreamRouteSwitchEvent[];
  /**
   * Metered units, and never an amount. Kaana measures units; what they cost is
   * the control plane's answer, and a second one quoted here would be
   * unauthoritative.
   */
  readonly units: readonly UsageQuantity[];
  readonly usageSource?: UsageSource;
  /**
   * The technical usage record. Absent when the connection dropped before Kaana
   * could deliver the frame — the work still happened and is still owed, and the
   * edge recovers the record by `requestId`.
   */
  readonly report?: NormalizedUsageReport;
}

async function* readBody(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<Uint8Array, void, undefined> {
  const reader = body.getReader();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      if (value !== undefined) yield value;
    }
  } finally {
    // Releasing the lock on an abandoned read is what lets the runtime tear the
    // connection down instead of holding it until the process exits.
    reader.releaseLock();
  }
}

/**
 * Reads a non-200 response as the contract's error body.
 *
 * A body that is not one is a protocol error and not an inference error: the
 * distinction is what stops a proxy's HTML error page being reported to a
 * customer as a refusal by the model.
 *
 * When the body says nothing, the echoed request id is all there is to correlate
 * on — and on a rejection it is one Kaana MINTED, because echoing an id from a
 * body it had not yet verified would let an unauthenticated caller choose what
 * appears in Kaana's logs.
 */
async function readFailure(response: Response): Promise<never> {
  const correlation = response.headers.get(KAANA_REQUEST_ID_HEADER);
  const unreadable =
    `Kaana answered ${response.status} with a body that is not a contract error` +
    (correlation === null ? '' : ` (${KAANA_REQUEST_ID_HEADER}: ${correlation})`);
  const text = await response.text().catch(() => '');
  let payload: unknown;
  try {
    payload = JSON.parse(text) as unknown;
  } catch {
    throw new KaanaProtocolError(unreadable);
  }
  const parsed = inferenceErrorSchema.safeParse(payload);
  if (!parsed.success) {
    throw new KaanaProtocolError(unreadable, { cause: parsed.error });
  }
  throw new KaanaInferenceError(parsed.data);
}

export class KaanaClient {
  readonly #baseUrl: string;
  readonly #signingKey: KaanaSigningKey;
  readonly #fetch: typeof globalThis.fetch;
  readonly #now: () => number;

  constructor(options: KaanaClientOptions) {
    // Trailing slashes are stripped once here rather than guarded at each join:
    // `https://host//internal/v1/inference` is a 404 whose cause is invisible.
    this.#baseUrl = options.baseUrl.replace(/\/+$/, '');
    this.#signingKey = options.signingKey;
    this.#fetch = options.fetch ?? globalThis.fetch;
    this.#now = options.now ?? Date.now;
  }

  /**
   * Posts the signed envelope and yields every frame Kaana sends, in order.
   *
   * The envelope is serialised ONCE and those exact bytes are both signed and
   * sent; anything else authenticates a body other than the one executed.
   */
  async *stream(
    request: InferenceRequest,
    options: KaanaCallOptions = {},
  ): AsyncGenerator<KaanaFrame, void, undefined> {
    const body = serializeInferenceRequest(request);
    const response = await this.#send(KAANA_INFERENCE_PATH, 'POST', body, options.signal);
    if (!response.ok) await readFailure(response);
    if (response.body === null) {
      throw new KaanaTransportError('Kaana accepted the envelope but sent no event stream');
    }
    for await (const frame of readSseFrames(readBody(response.body))) {
      yield decodeKaanaFrame(frame);
    }
  }

  /**
   * Drains a stream into one answer.
   *
   * Throws {@link KaanaInferenceError} on the stream's terminal error event, so
   * a caller never has to check for a failure that arrived as a successful HTTP
   * response — which is every failure past the envelope's admission.
   */
  async complete(
    request: InferenceRequest,
    options: KaanaCallOptions = {},
  ): Promise<KaanaCompletion> {
    let start: InferenceStreamStartEvent | undefined;
    let finishReason: InferenceFinishReason | undefined;
    let generationId: string | undefined;
    let report: NormalizedUsageReport | undefined;
    let usageSource: UsageSource | undefined;
    let units: readonly UsageQuantity[] = [];
    const routeSwitches: InferenceStreamRouteSwitchEvent[] = [];
    const channels = { output_text: '', reasoning: '', refusal: '' };
    // Insertion-ordered, so tool calls come back in the order the model made
    // them rather than in the order their ids happen to sort.
    const toolCalls = new Map<string, { name: string; arguments: string; complete: boolean }>();

    for await (const frame of this.stream(request, options)) {
      if (frame.frame === KAANA_FRAME_USAGE_REPORT) {
        report = frame.report;
        continue;
      }
      if (frame.frame !== KAANA_FRAME_STREAM_EVENT) continue;
      const event = frame.event;
      switch (event.type) {
        case 'start':
          start = event;
          generationId = event.generationId ?? generationId;
          break;
        case 'delta':
          channels[event.channel] += event.text;
          break;
        case 'tool_call': {
          const existing = toolCalls.get(event.toolCallId) ?? {
            name: '',
            arguments: '',
            complete: false,
          };
          toolCalls.set(event.toolCallId, {
            name: event.name ?? existing.name,
            arguments: existing.arguments + (event.argumentsDelta ?? ''),
            complete: existing.complete || event.complete,
          });
          break;
        }
        case 'usage':
          units = event.units;
          usageSource = event.usageSource;
          break;
        case 'route_switch':
          routeSwitches.push(event);
          break;
        case 'error':
          // Terminal: no `done` follows, so a caller that saw an error never
          // also has to reconcile a success.
          throw new KaanaInferenceError(event.error);
        case 'done':
          finishReason = event.finishReason;
          generationId = event.generationId ?? generationId;
          break;
      }
    }

    if (start === undefined || finishReason === undefined) {
      // A stream that ended without a terminal event is a truncated one. Saying
      // so is the difference between a retryable transport failure and a model
      // that answered with nothing.
      throw new KaanaTransportError(
        `the stream for ${request.attribution.requestId} ended without a terminal event`,
      );
    }

    return {
      requestId: request.attribution.requestId,
      generationId,
      start,
      text: channels.output_text,
      reasoning: channels.reasoning,
      refusal: channels.refusal,
      toolCalls: [...toolCalls].map(([toolCallId, call]) => ({ toolCallId, ...call })),
      finishReason,
      routeSwitches,
      units,
      usageSource,
      report,
    };
  }

  /**
   * The signed health projection.
   *
   * A GET, but still signed — `internal/httpapi` runs the same `readSignedBody`
   * on it, so the signature covers an EMPTY body and its digest is `sha256("")`.
   */
  async health(options: KaanaCallOptions = {}): Promise<KaanaHealth> {
    const response = await this.#send(KAANA_HEALTH_PATH, 'GET', new Uint8Array(0), options.signal);
    if (!response.ok) await readFailure(response);
    const parsed = kaanaHealthSchema.safeParse(await response.json());
    if (!parsed.success) {
      throw new KaanaProtocolError('the health projection is not the shape this build reads', {
        cause: parsed.error,
      });
    }
    return parsed.data;
  }

  async #send(
    path: string,
    method: 'GET' | 'POST',
    body: Uint8Array,
    signal: AbortSignal | undefined,
  ): Promise<Response> {
    const headers: Record<string, string> = {
      ...signEdgeRequest(this.#signingKey, body, this.#now()),
      Accept: 'text/event-stream, application/json',
    };
    if (method === 'POST') headers['Content-Type'] = 'application/json';
    try {
      return await this.#fetch(`${this.#baseUrl}${path}`, {
        method,
        headers,
        // A GET carries no body at all; the empty one signed above is what the
        // health route verifies against.
        body: method === 'POST' ? body : undefined,
        signal,
      });
    } catch (cause) {
      // The path, never the headers: they carry the signature, and a caller's
      // own logging of a thrown error is exactly where they would end up.
      throw new KaanaTransportError(`the request to ${path} did not complete`, { cause });
    }
  }
}
