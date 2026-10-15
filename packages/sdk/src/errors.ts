/**
 * Typed errors (Kaana ADR 0002, "Superficie del SDK").
 *
 * `code` is the PUBLIC vocabulary of `@kaana.ai/contracts`: it says what went
 * wrong and whether to retry, never which provider served or failed. Accounts,
 * keys, limits and balances are managed in Oxy Console (https://console.oxy.so).
 */

import {
  NON_RETRYABLE_PUBLIC_ERROR_CODES,
  type PublicError,
  type PublicErrorCode,
} from '@kaana.ai/contracts';

const NON_RETRYABLE: ReadonlySet<string> = new Set(NON_RETRYABLE_PUBLIC_ERROR_CODES);

/** Whether an identical retry of a request that failed with `code` can succeed. */
export function isRetryableCode(code: PublicErrorCode): boolean {
  return !NON_RETRYABLE.has(code);
}

export interface KaanaErrorInit {
  code: PublicErrorCode;
  message: string;
  /** HTTP status, or null when the error arrived as a terminal stream event. */
  status: number | null;
  requestId: string | null;
  retryable: boolean;
  retryAfterMs?: number;
  param?: string;
  /** Whether output reached the caller before the failure. */
  afterOutput?: boolean;
}

/** A failure the API reported in its public vocabulary. */
export class KaanaError extends Error {
  readonly code: PublicErrorCode;
  readonly status: number | null;
  readonly requestId: string | null;
  readonly retryable: boolean;
  readonly retryAfterMs?: number;
  readonly param?: string;
  readonly afterOutput: boolean;

  constructor(init: KaanaErrorInit) {
    super(init.message);
    this.name = new.target.name;
    this.code = init.code;
    this.status = init.status;
    this.requestId = init.requestId;
    this.retryable = init.retryable;
    if (init.retryAfterMs !== undefined) this.retryAfterMs = init.retryAfterMs;
    if (init.param !== undefined) this.param = init.param;
    this.afterOutput = init.afterOutput ?? false;
  }
}

/** `authentication_failed`: the credential is missing, malformed, expired or revoked. */
export class KaanaAuthenticationError extends KaanaError {}
/** `permission_denied`, `insufficient_scope`, `commercial_permission_denied`, `policy_violation`. */
export class KaanaPermissionError extends KaanaError {}
/** The request itself cannot be served as written. */
export class KaanaInvalidRequestError extends KaanaError {}
/** `insufficient_balance`, `spending_limit_exceeded`, `quota_exceeded`. */
export class KaanaBillingError extends KaanaError {}
/** `rate_limited`. */
export class KaanaRateLimitError extends KaanaError {}
/** No route could serve the request right now, or the upstream failed. */
export class KaanaUnavailableError extends KaanaError {}
/** `upstream_content_filtered`. */
export class KaanaContentFilteredError extends KaanaError {}
/** `cancelled`, including a request the caller aborted. */
export class KaanaCancelledError extends KaanaError {}
/** `internal_error`. */
export class KaanaInternalError extends KaanaError {}

/** A response that does not satisfy `@kaana.ai/contracts`. Never retryable. */
export class KaanaProtocolError extends Error {
  readonly requestId: string | null;

  constructor(message: string, requestId: string | null) {
    super(message);
    this.name = 'KaanaProtocolError';
    this.requestId = requestId;
  }
}

/** The network failed, or the request timed out, before a response arrived. */
export class KaanaConnectionError extends Error {
  override readonly cause: unknown;
  readonly timedOut: boolean;

  constructor(message: string, options: { cause?: unknown; timedOut?: boolean } = {}) {
    super(message);
    this.name = 'KaanaConnectionError';
    this.cause = options.cause;
    this.timedOut = options.timedOut ?? false;
  }
}

type ErrorClass = new (init: KaanaErrorInit) => KaanaError;

const CLASS_BY_CODE: Readonly<Record<PublicErrorCode, ErrorClass>> = {
  authentication_failed: KaanaAuthenticationError,
  permission_denied: KaanaPermissionError,
  insufficient_scope: KaanaPermissionError,
  commercial_permission_denied: KaanaPermissionError,
  policy_violation: KaanaPermissionError,
  invalid_request: KaanaInvalidRequestError,
  model_not_found: KaanaInvalidRequestError,
  unsupported_modality: KaanaInvalidRequestError,
  context_length_exceeded: KaanaInvalidRequestError,
  request_too_large: KaanaInvalidRequestError,
  output_limit_exceeded: KaanaInvalidRequestError,
  idempotency_conflict: KaanaInvalidRequestError,
  byok_credential_invalid: KaanaInvalidRequestError,
  insufficient_balance: KaanaBillingError,
  spending_limit_exceeded: KaanaBillingError,
  quota_exceeded: KaanaBillingError,
  rate_limited: KaanaRateLimitError,
  no_route_available: KaanaUnavailableError,
  service_unavailable: KaanaUnavailableError,
  provider_error: KaanaUnavailableError,
  provider_timeout: KaanaUnavailableError,
  provider_overloaded: KaanaUnavailableError,
  upstream_content_filtered: KaanaContentFilteredError,
  cancelled: KaanaCancelledError,
  internal_error: KaanaInternalError,
};

/** The typed error for a public code. */
export function kaanaError(init: KaanaErrorInit): KaanaError {
  const ErrorType = CLASS_BY_CODE[init.code] ?? KaanaError;
  return new ErrorType(init);
}

/** The typed error for a `PublicError` body or terminal event. */
export function fromPublicError(
  error: PublicError,
  status: number | null,
  afterOutput = false,
): KaanaError {
  return kaanaError({
    code: error.code,
    message: error.message,
    status,
    requestId: error.requestId,
    retryable: error.retryable,
    ...(error.retryAfterMs === undefined ? {} : { retryAfterMs: error.retryAfterMs }),
    ...(error.param === undefined ? {} : { param: error.param }),
    afterOutput,
  });
}

/** A request the SDK refuses before sending anything. */
export function clientRefusal(param: string, message: string): KaanaInvalidRequestError {
  return new KaanaInvalidRequestError({
    code: 'invalid_request',
    message,
    status: null,
    requestId: null,
    retryable: false,
    param,
  });
}

/** The caller withdrew the request. */
export function cancelledByCaller(requestId: string | null, afterOutput = false): KaanaError {
  return new KaanaCancelledError({
    code: 'cancelled',
    message: 'the request was aborted by the caller',
    status: null,
    requestId,
    retryable: false,
    afterOutput,
  });
}
