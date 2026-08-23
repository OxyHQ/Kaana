/**
 * The five things that can go wrong on this hop, kept apart because a caller
 * acts differently on each.
 *
 * Collapsing them would leave a string match on a message as the only
 * distinction available at a call site, and the interesting question here is not
 * "did it fail" but "whose fault is it and can it be retried": an envelope this
 * client refused to build is a bug in the caller, a transport failure may be
 * retried blind, and a {@link KaanaInferenceError} carries the contract's own
 * `retryable` assertion, which is a producer statement and must not be
 * second-guessed by re-deriving it from the code.
 *
 * No error here ever carries a request header, a signing key, or a response body
 * it has not classified. The Oxy edge's private key is the one secret that
 * passes through this package, and an error message is the classic way one
 * reaches a log aggregator.
 */

import type { InferenceError } from '@oxyhq/contracts';

/** Base class, so a caller can catch everything this package throws at once. */
export class KaanaError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = new.target.name;
  }
}

/**
 * The envelope the caller asked for is not a valid inference request.
 *
 * Thrown before anything is sent. `cause` is the ZodError, which names the
 * failing path — this package does not restate it, because a re-worded copy of a
 * validation message is a second version of it that drifts.
 */
export class KaanaEnvelopeError extends KaanaError {}

/**
 * The call produced no readable HTTP response: a network failure, an abort, or
 * a response Kaana ended without a body.
 */
export class KaanaTransportError extends KaanaError {}

/**
 * Kaana answered, but not in a shape this contract version reads — a frame whose
 * payload is not JSON, or a stream event that does not parse.
 *
 * Fatal to the stream rather than skipped, deliberately. An event a consumer
 * silently drops is either output the customer paid for and never saw, or a
 * terminal event that terminated nothing.
 */
export class KaanaProtocolError extends KaanaError {}

/**
 * Kaana refused, or the request failed: the contract's own error body, from a
 * non-200 response or from the stream's terminal `error` event.
 */
export class KaanaInferenceError extends KaanaError {
  /** The contract error body, verbatim. */
  readonly failure: InferenceError;

  constructor(failure: InferenceError) {
    super(`${failure.code}: ${failure.message}`);
    this.failure = failure;
  }

  /** The producer's assertion, never re-derived from the code on this side. */
  get retryable(): boolean {
    return this.failure.retryable;
  }
}
