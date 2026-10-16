/**
 * Version rule for the Oxy↔data-plane inference contracts.
 *
 * Oxy is the control plane; the inference data plane is a separate service.
 * They are deployed independently, in different repositories, possibly in
 * different languages.
 * Every shape they exchange therefore carries its version IN THE PARSED DATA,
 * never as a comment or an out-of-band assumption, so a producer running ahead
 * of a consumer fails loudly at the parse instead of being silently reinterpreted.
 *
 * The rule, enforced by `src/__tests__/inference.compatibility.test.ts`:
 *
 *  - A schema carries `schemaVersion: z.literal(<n>)` **if and only if** it can
 *    appear on the wire as a whole message — a request envelope, a stream event,
 *    a catalogue descriptor, a ledger record, an error body.
 *  - A schema that only ever appears EMBEDDED inside such a message (the
 *    attribution block, one message part, one usage quantity, a data-retention
 *    policy) carries no version of its own: it inherits the version of the
 *    envelope it rides in. Versioning it separately would create two versions
 *    that can disagree about one byte stream.
 *  - A shape that is BOTH — `inferenceErrorSchema` is returned as an HTTP body
 *    and also rides inside the stream's error event — keeps its own version.
 *    The envelope's version then governs the envelope and the payload's governs
 *    the payload, which is two versions of two things rather than two versions
 *    of one.
 *  - Every exported object schema in `src/inference/` must fall into exactly one
 *    of those groups. The compatibility test holds both lists as exact
 *    equalities, so a new shape that is in neither fails the build rather than
 *    quietly shipping unversioned.
 *
 * A shape's own version is bumped when its meaning changes in a way a consumer
 * pinned to the previous version would misread — a field removed, a field's
 * units changed, a closed enum's member given a new meaning. Adding an OPTIONAL
 * field is additive and does not bump it, because a consumer on the previous
 * version parses the message correctly and simply does not read the new field.
 *
 * ## Which shapes reject an unknown field
 *
 * That last rule is why the shapes EXCHANGED WITH THE DATA PLANE are not
 * `.strict()` at their top level — the request envelope, the four usage records,
 * the stream events, the error body, the catalogue descriptors, the price
 * version. The split is a decision rather than an omission: `.strict()` and
 * "adding an optional field is additive" cannot both hold on one shape, because
 * a producer one minor version ahead would have its whole message REFUSED
 * rather than its new field ignored. For a usage report that means a request
 * already served upstream can never be settled and Oxy absorbs its cost, which
 * is a worse failure than the one strictness would have caught.
 *
 * Their LEAVES are strict, and that is where the protection lives: a stripped
 * field is the worse outcome exactly where it would be a leak or a second
 * source of truth, because it disappears at this parse and survives in the
 * producer, which is where somebody eventually reads it. So
 * `clientRequestMetadataSchema` (no IP, ever), `moneySchema` (no convenience
 * float beside the exact decimal), `providerErrorPassthroughSchema` (no
 * upstream request or headers beside the message), `usageQuantitySchema` and
 * `unitPriceSchema` all refuse an unknown field, while the envelope carrying
 * them tolerates an additive one.
 *
 * A shape Oxy does NOT exchange with the data plane is strict at its top level
 * too, since nothing there can run ahead of this package:
 * `providerConnectionSchema`, where an unknown field is how a BYOK credential
 * escapes, and the billing and entitlement records, where one is a second
 * number beside an exact amount.
 *
 * A SIGNED document is strict at its top level for a third reason, and there it
 * is forced rather than chosen. `aliaModelReleaseManifestSchema` carries
 * signatures over its own canonical bytes, so a field stripped at this parse is a
 * field missing from the bytes a verifier re-canonicalizes: a tolerant parse
 * would report an invalid SIGNATURE where the truth is that this build does not
 * understand the DOCUMENT. Its producer can run ahead of this package — Alia's
 * release tooling is deployed independently — so the usual argument applies here
 * and is outweighed, because the refusal costs an operator one retry after Oxy
 * takes the newer contract, while the tolerant parse costs a misdiagnosis of a
 * cryptographic failure.
 *
 * Decided in: Oxy docs/adr/0006-oxy-kaana-boundary.md,
 * docs/adr/0010-public-api-compatibility.md,
 * docs/adr/0017-authorized-routes-in-the-envelope.md; moved from
 * `@oxy.so/contracts` 4.12.0 (`src/inference/version.ts`) by Kaana ADR 0002.
 */

/**
 * Version of the contract SET as a whole — the value the control plane and the
 * data plane exchange in a startup/health handshake to establish that they were built against
 * compatible definitions before a single inference request is served.
 *
 * MAJOR is bumped when any individual shape's `schemaVersion` increments (at
 * least one message is now read differently by the two sides); MINOR when a
 * shape or an optional field is added, when a CLOSED ENUM gains a member, or
 * when a refinement changes which bytes parse; PATCH for documentation-only
 * changes that leave every parsed byte identical.
 *
 * The last two are MINOR rather than PATCH because both produce the same
 * failure: a producer on the newer set emits something the older set refuses,
 * with no `schemaVersion` difference to explain it. A new enum member and a
 * loosened refinement are exactly what the handshake exists to surface — a
 * skew the per-message version cannot express.
 *
 * This constant is deliberately NOT embedded in the request envelope. Pinning a
 * request to the version of the whole set would make an unrelated additive
 * change to, say, the catalogue reject every in-flight inference request; the
 * per-shape `schemaVersion` is what a message is validated against.
 */
export const INFERENCE_CONTRACT_VERSION = '5.0.0';

/**
 * Contract set 4.0.0 (Kaana ADR 0002) is the first published as
 * `@kaana.ai/contracts`. MAJOR because the set changed hands and split in two:
 * the public `https://kaana.ai/v1` shapes at the package root, and the
 * Oxy↔Kaana shapes under `@kaana.ai/contracts/internal`. Against `@oxy.so/contracts`
 * 4.12.0 (set 3.8.0) every moved shape parses the same bytes, with two
 * deliberate exceptions: the signed envelope admits `audio_speech` with
 * `stream: true`, and the economic treatment gains `platform_fee_only`.
 *
 * Contract set 4.1.0 (Alia ADR 0017, Kaana phase 1) is MINOR: the closed usage
 * unit set gains `input_transcription_milliseconds` (input transcription
 * inside a realtime conversation, billed by the deployment's transcription
 * model), `capabilities.realtime` gains the optional `inputAudioTranscription`
 * and `audioFormats`, and `realtimeAudioFormatSchema` moves from `realtime.ts`
 * to `audioFormats.ts` with the same members. A consumer on 4.0.0 refuses a
 * usage report carrying the new unit, which is why the signed-envelope lane
 * keeps refusing input transcription (docs/realtime.md).
 *
 * Contract set 4.2.0 (Kaana ADR 0002 phase 3) is additive: the JSON form of a
 * transcription upload, the transcription duration ceiling, and the response
 * headers and realtime subprotocols `https://kaana.ai/v1` speaks.
 *
 * Contract set 4.3.0 (Kaana ADR 0002, amendment 2026-10-15) is additive: the
 * internal `declaredModelSchema` and `declaredCatalogueSchema`, the catalogue
 * Kaana declares (identity, licence, data policy, capabilities and the list
 * price per unit), carried to Oxy as `declaredCatalogue` on the signed
 * `GET /internal/v1/models`. Nothing in 4.2.0 changes.
 *
 * Contract set 4.4.0 (Kaana ADR 0002, amendment 2026-10-15) is additive: the
 * internal `deploymentCommercialSchema`, the optional `commercial` member of
 * every deployment descriptor Kaana serves (availability scope, commercial
 * permission, dedicated capacity), and `sameModelFailover` on
 * `grantStepLimitsSchema` (default `true`: what 4.3.0 did). Nothing in 4.3.0
 * changes.
 *
 * Contract set 4.5.0 is additive: the signed envelope's optional
 * `transcription` member (`inferenceTranscriptionParametersSchema`), which
 * carries a file transcription's `language` to the model instead of the
 * public API refusing it. Nothing in 4.4.0 changes.
 *
 * Contract set 5.0.0 publishes what each voice model ACCEPTS, so a client
 * builds its request from the catalogue instead of learning the rules from a
 * refusal. Additive: `capabilities.realtime.parameters` (turn detections and
 * their bounds, output modality sets, tool choices, the output token cap, the
 * input transcription prompt, and the units each detection is billed in),
 * `audio.detectsLanguage` and `audio.transcriptionPrompt`,
 * `realtimeParameterIssues` and `checkTranscriptionRequest`, the envelope's
 * `transcription.prompt`, `MAX_REALTIME_OPEN_FRAME_BYTES`. MAJOR because it
 * REMOVES what no server ever served, each of which Kaana already refused:
 * the realtime `temperature`, a realtime tool's `strict`
 * (`realtimeToolDefinitionSchema`), `limits.maxTextItems` and
 * `MAX_REALTIME_TEXT_ITEMS`, and a transcription's `timestamps`, `segments`
 * and `words`. A request that parsed under 4.5.0 and that Kaana served
 * still parses and is served the same.
 */

/** The public API's one origin and version. */
export const KAANA_API_BASE_URL = 'https://kaana.ai/v1';

/** The response header that carries the request id. */
export const KAANA_REQUEST_ID_HEADER = 'Kaana-Request-Id';

/**
 * The header that carries {@link INFERENCE_CONTRACT_VERSION}: on every
 * response, the set Kaana serves; on a request (`@kaana.ai/sdk` 2.0.0 and
 * later), the set the client parses. The catalogue's leaves are strict, so
 * Kaana renders `GET /v1/models` in the shape of the client's set: a client
 * that states none is served the 4.x shape, without what set 5.0.0 added.
 */
export const KAANA_CONTRACT_VERSION_HEADER = 'Kaana-Contract-Version';

/**
 * The response header that names the revision that actually served a request
 * whose body is not JSON (`POST /v1/audio/speech` without SSE), e.g.
 * `elevenlabs/eleven-multilingual@v2`.
 */
export const KAANA_MODEL_HEADER = 'Kaana-Model';

/**
 * The response header that carries the usage of a request whose body is not
 * JSON (`POST /v1/audio/speech` without streaming): `unit=quantity` pairs
 * joined by `,`, each unit once, e.g. `characters=12,requests=1`.
 */
export const KAANA_USAGE_HEADER = 'Kaana-Usage';

/**
 * The response headers that carry the customer's routing-policy revision on
 * `POST /v1/responses`, so a streamed response can be folded into a
 * `Response` without a second request.
 */
export const KAANA_ROUTING_POLICY_ID_HEADER = 'Kaana-Routing-Policy-Id';
export const KAANA_ROUTING_POLICY_VERSION_HEADER = 'Kaana-Routing-Policy-Version';

/** The request header that names the end user a service token acts for. */
export const KAANA_ON_BEHALF_OF_HEADER = 'X-Oxy-User-Id';

/** The request header that makes a request idempotent. */
export const KAANA_IDEMPOTENCY_KEY_HEADER = 'Idempotency-Key';

/**
 * The WebSocket subprotocol of `GET /v1/realtime`. A client offers it, and the
 * server selects it, on every connection.
 */
export const KAANA_REALTIME_SUBPROTOCOL = 'kaana.v1';

/**
 * A runtime whose WebSocket cannot set `Authorization` (a browser, React
 * Native, Node's global WebSocket) carries the credential as a second offered
 * subprotocol: this prefix followed by the unpadded base64url of the
 * credential's UTF-8 bytes. The server never selects it.
 */
export const KAANA_REALTIME_BEARER_PROTOCOL_PREFIX = 'kaana.bearer.';

/**
 * Likewise for `X-Oxy-User-Id`: this prefix followed by the unpadded base64url
 * of the user id. Honoured only with a service token, as the header is.
 */
export const KAANA_REALTIME_USER_PROTOCOL_PREFIX = 'kaana.user.';
