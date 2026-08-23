/**
 * `@oxyhq/kaana-client` — the TypeScript client for Kaana's inference data
 * plane, published from the data plane's own repository so every Oxy caller
 * consumes one implementation of the wire rather than a fifth private copy.
 *
 * Three layers, kept apart because they have different owners and drift
 * differently:
 *
 *  - The WIRE SHAPES are `@oxyhq/contracts`'. That package is the contract;
 *    `internal/contract` restates it in Go and is answerable to it, and this
 *    package imports it directly. Nothing here re-states a wire shape, so there
 *    is no third copy to keep in step.
 *  - The TRANSPORT — Ed25519 edge signing, the SSE frame names, the health
 *    projection — is this repository's own and is published nowhere else. It is
 *    declared here, against `internal/edgeauth`, `internal/sse` and
 *    `internal/httpapi`, and that is what makes this a client rather than a
 *    wrapper.
 *  - The CLIENT is the assembly: serialise once, sign and send those exact
 *    bytes, read the stream, and turn a terminal error event into a thrown
 *    error.
 *
 * ## The boundary this package keeps
 *
 * Kaana measures units and never prices them. Nothing exported below is an
 * amount, a price, a balance, a receipt or a ledger record — those are the
 * control plane's, and `test/boundary.test.ts` asserts the absence rather than
 * leaving it to review, deriving the forbidden vocabulary from the published
 * contract's own money and billing modules instead of a list somebody typed.
 *
 * Oxy identifiers ride the envelope as immutable opaque strings. This package
 * never parses one, stores one, or decides anything from one.
 *
 * ## The `relay` spelling
 *
 * It survives in the header names, the signing domain and the route paths.
 * They are a wire format a running service checks byte for byte, so renaming
 * them on one side alone would refuse every envelope.
 */

export {
  KaanaClient,
  KAANA_HEALTH_PATH,
  KAANA_INFERENCE_PATH,
  KAANA_REQUEST_ID_HEADER,
  type KaanaCallOptions,
  type KaanaClientOptions,
  type KaanaCompletion,
  type KaanaToolCall,
} from './client.js';

export {
  buildInferenceRequest,
  serializeInferenceRequest,
  type KaanaAttributionInput,
  type KaanaClientMetadataInput,
  type KaanaInputInput,
  type KaanaMessageInput,
  type KaanaRequestInput,
} from './envelope.js';

export {
  KaanaEnvelopeError,
  KaanaError,
  KaanaInferenceError,
  KaanaProtocolError,
  KaanaTransportError,
} from './errors.js';

export {
  kaanaDeploymentHealthSchema,
  kaanaHealthSchema,
  kaanaKeyHealthSchema,
  kaanaKeyPoolHealthSchema,
  kaanaProviderHealthSchema,
  kaanaProviderHealthStatusSchema,
  kaanaSnapshotStatusSchema,
  type KaanaDeploymentHealth,
  type KaanaHealth,
  type KaanaProviderHealth,
  type KaanaSnapshotStatus,
} from './health.js';

export {
  ed25519PrivateKeyFromSeed,
  ed25519PublicKeyFromRaw,
  ed25519PublicKeyToRaw,
  edgeSigningInput,
  signEdgeRequest,
  EDGE_SIGNATURE_DOMAIN,
  EDGE_SIGNATURE_HEADERS,
  EDGE_SIGNATURE_MAX_SKEW_MS,
  EDGE_SIGNATURE_PREFIX,
  type KaanaSigningKey,
} from './signing.js';

export { readSseFrames, type SseFrame } from './sse.js';

export {
  decodeKaanaFrame,
  KAANA_FRAME_STREAM_EVENT,
  KAANA_FRAME_USAGE_REPORT,
  type KaanaFrame,
  type KaanaStreamEventFrame,
  type KaanaUnknownFrame,
  type KaanaUsageReportFrame,
} from './stream.js';
