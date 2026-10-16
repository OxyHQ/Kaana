/**
 * `@kaana.ai/sdk` — the client of Kaana's public API, `https://kaana.ai/v1`.
 *
 * Applications, `oxy_sk_…` keys, limits, budgets and billing live in Oxy
 * Console (https://console.oxy.so). The credential is a function or a string;
 * the SDK never stores it and has no Oxy dependency.
 */

export type { SpeechResult, SpeechStream, TranscriptionRequest } from './audio.js';
export { encodeBase64, decodeBase64 } from './base64.js';
export { Kaana } from './client.js';
export {
  KaanaAuthenticationError,
  KaanaBillingError,
  KaanaCancelledError,
  KaanaConnectionError,
  KaanaContentFilteredError,
  KaanaError,
  type KaanaErrorInit,
  KaanaInternalError,
  KaanaInvalidRequestError,
  KaanaPermissionError,
  KaanaProtocolError,
  KaanaRateLimitError,
  KaanaUnavailableError,
} from './errors.js';
export type {
  KaanaCredential,
  KaanaFetch,
  KaanaOptions,
  RealtimeConnectOptions,
  RequestOptions,
} from './http.js';
export {
  MAX_REALTIME_AUDIO_FRAME_BYTES,
  type RealtimeClientEventInput,
  type RealtimeConnectRequest,
  type RealtimeSession,
} from './realtime.js';
export type { KaanaStream } from './stream.js';
