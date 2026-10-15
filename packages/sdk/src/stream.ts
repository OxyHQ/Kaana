/**
 * The stream handle every streaming method returns: an async iterable of
 * validated events that also resolves to the folded result.
 */

import { cancelledByCaller } from './errors.js';

/**
 * Validated events; `final` resolves with the folded response or rejects with
 * the KaanaError of the terminal `error` event.
 *
 * Iterate it, or await `final` alone (which drains it). A stream is consumed
 * once: iterate it before reading `final` if you want both. Breaking out of
 * the loop or calling `abort()` cancels the request upstream.
 */
export interface KaanaStream<E, F> extends AsyncIterable<E> {
  readonly requestId: Promise<string>;
  readonly final: Promise<F>;
  abort(reason?: unknown): void;
}

export interface SourceContext {
  readonly signal: AbortSignal;
  setRequestId(requestId: string): void;
  /** Called by the source once output reached the caller. */
  markOutput(): void;
}

export type Source<E, F> = (context: SourceContext) => AsyncGenerator<E, F, undefined>;

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
  reject(reason: unknown): void;
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((onResolve, onReject) => {
    resolve = onResolve;
    reject = onReject;
  });
  // The rejection is the caller's to observe; an unobserved one must not crash
  // the process as an unhandled rejection.
  promise.catch(() => undefined);
  return { promise, resolve, reject };
}

/** Link the caller's signal to a controller the stream owns. */
export function linkSignal(controller: AbortController, caller: AbortSignal | undefined): void {
  if (caller === undefined) return;
  if (caller.aborted) {
    controller.abort(caller.reason);
    return;
  }
  caller.addEventListener('abort', () => controller.abort(caller.reason), { once: true });
}

export class StreamHandle<E, F> implements KaanaStream<E, F> {
  readonly #controller = new AbortController();
  readonly #requestId = deferred<string>();
  readonly #final = deferred<F>();
  readonly #source: Source<E, F>;
  #claimed = false;
  #requestIdValue: string | null = null;
  #output = false;

  constructor(source: Source<E, F>, callerSignal: AbortSignal | undefined) {
    this.#source = source;
    linkSignal(this.#controller, callerSignal);
  }

  get requestId(): Promise<string> {
    return this.#requestId.promise;
  }

  get final(): Promise<F> {
    if (!this.#claimed) {
      this.#claimed = true;
      void this.#drain();
    }
    return this.#final.promise;
  }

  abort(reason?: unknown): void {
    this.#controller.abort(reason);
  }

  [Symbol.asyncIterator](): AsyncIterator<E> {
    if (this.#claimed) {
      throw new TypeError('A Kaana stream is consumed once: iterate it before reading final.');
    }
    this.#claimed = true;
    return this.#iterate();
  }

  async #drain(): Promise<void> {
    try {
      for await (const _event of this.#iterate()) {
        // Draining: the folded result is what the caller asked for.
      }
    } catch (_error) {
      // Already delivered through `final`.
    }
  }

  async *#iterate(): AsyncGenerator<E, void, undefined> {
    const source = this.#source({
      signal: this.#controller.signal,
      setRequestId: (requestId) => {
        this.#requestIdValue = requestId;
        this.#requestId.resolve(requestId);
      },
      markOutput: () => {
        this.#output = true;
      },
    });
    let settled = false;
    try {
      while (true) {
        const next = await source.next();
        if (next.done) {
          settled = true;
          this.#final.resolve(next.value);
          return;
        }
        yield next.value;
      }
    } catch (error) {
      settled = true;
      this.#requestId.reject(error);
      this.#final.reject(error);
      throw error;
    } finally {
      if (!settled) {
        // The caller stopped iterating: the request is cancelled upstream.
        this.#controller.abort();
        await source.return(undefined as F).catch(() => undefined);
        const cancelled = cancelledByCaller(this.#requestIdValue, this.#output);
        this.#requestId.reject(cancelled);
        this.#final.reject(cancelled);
      }
    }
  }
}
