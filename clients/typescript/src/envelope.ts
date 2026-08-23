/**
 * Building the request envelope the Oxy edge forwards to Kaana.
 *
 * ## Where the types come from
 *
 * `@oxyhq/contracts` is the wire contract; `internal/contract` restates it in Go
 * and is answerable to it. This file is the third consumer of that same
 * authority, not a third statement of it: every wire type below is imported from
 * the published package, and validation is `inferenceRequestSchema` itself. A
 * hand-copied mirror of the Go restatement would be a copy of a copy, drifting
 * from both while continuing to parse.
 *
 * What the builder adds is the part that is easy to get wrong by hand, where
 * none of the mistakes errors at the point it is made:
 *
 *  - `client.apiFormat` is REQUIRED, and its absence is refused at Kaana's parse
 *    rather than at the caller's.
 *  - `input.format: 'text'` is the EMBEDDING input. A chat turn is
 *    `format: 'messages'`, and sending the first where the second was meant
 *    produces a confusing upstream refusal instead of a validation error.
 *  - `routingPolicy` is `{ routingPolicyId, policyVersion }` — a REFERENCE to
 *    the customer's own policy at a revision, not a slug. `Request.Validate` in
 *    `internal/contract` deliberately checks nothing about it, since the edge
 *    already decided; so an envelope carrying the wrong shape is accepted and
 *    served, and the request becomes one whose admitting policy cannot be
 *    reconstructed afterwards. This builder validates it here instead.
 *  - `sampling` and `tools` are required properties with empty defaults, not
 *    optional ones.
 *
 * ## Attribution is carried, never owned
 *
 * `accountId`, `applicationId`, `credentialId` and `userId` are immutable opaque
 * strings that ride the envelope. This package never parses one, joins on one,
 * stores one, or derives an authorization decision from one — that is the Oxy
 * edge's, resolved before a request is forwarded. `inferenceScopes` is passed
 * through for the same reason: Kaana reads it for exactly one thing, refusing an
 * envelope that was never authorized to invoke inference at all.
 *
 * ## A routing-profile target is well-formed and currently unservable
 *
 * `RoutingTarget` carries both of the contract's arms, and this builder accepts
 * either, because the envelope is the contract's. But `internal/kaana`'s
 * executor refuses every `routing_profile` target with `invalid_request` and
 * `param: target.routingProfile`, and that is a property of the build rather
 * than a configuration gap: resolving a profile needs its candidate list, which
 * lives in the Oxy catalogue, and the envelope carries a routing policy
 * REFERENCE rather than a snapshot. Choosing a model there would be the silent
 * substitution the platform forbids. So a caller wanting a model today names one
 * — in canonical `<publisher>/<model>` form, since a bare id fails the reference
 * grammar and this builder refuses it before it reaches the wire.
 *
 * ## Modality
 *
 * The contract carries five modalities and this builder accepts all of them,
 * because the envelope is the contract's and not one deployment's.
 * `internal/provider/openaicompat` refuses anything but `text` with
 * `unsupported_modality`, so an audio or image envelope is well-formed and
 * currently unservable. Refusing it here would put a second, staler copy of the
 * adapter set's capabilities in a client library.
 */

import {
  inferenceRequestSchema,
  type ClientRequestMetadata,
  type InferenceAttribution,
  type InferenceContentPart,
  type InferenceMessage,
  type InferenceModality,
  type InferenceRequest,
  type InferenceScope,
  type ResponseFormat,
  type RoutingPolicyReference,
  type RoutingTarget,
  type SamplingParameters,
  type ToolChoice,
  type ToolDefinition,
} from '@oxyhq/contracts';
import type { z } from 'zod';

import { KaanaEnvelopeError } from './errors.js';

/** What the schema accepts before branding and defaults are applied. */
type InferenceRequestInput = z.input<typeof inferenceRequestSchema>;

/**
 * A message, with `content` allowed to be a plain string.
 *
 * The contract's message content is always a list of parts, and writing
 * `[{ type: 'text', text }]` for every ordinary turn is the kind of ceremony
 * that grows a local helper in each caller. The string form is expanded here,
 * once.
 */
export interface KaanaMessageInput extends Omit<InferenceMessage, 'content'> {
  readonly content: string | readonly InferenceContentPart[];
}

/** The attribution block, flattened: a caller holds these as separate values. */
export interface KaanaAttributionInput {
  /** The Oxy account that is charged. Never the delegated user. */
  readonly accountId: string;
  readonly applicationId: string;
  readonly credentialId: string;
  readonly environment: InferenceAttribution['principal']['environment'];
  /**
   * The inference scopes the credential carries. Defaults to `inference:invoke`
   * alone — the one scope Kaana reads, and the only one that makes an envelope
   * servable at all.
   */
  readonly inferenceScopes?: readonly InferenceScope[];
  /** The delegated end user. Attribution only: it never changes who pays. */
  readonly userId?: string;
  readonly requestId: string;
  readonly generationId?: string;
}

/** The three input shapes, as an exclusive choice rather than a format string. */
export type KaanaInputInput =
  | { readonly messages: readonly KaanaMessageInput[] }
  | { readonly text: string }
  | { readonly texts: readonly string[] };

/** `client`, with `receivedAt` defaulted to now. */
export interface KaanaClientMetadataInput extends Omit<ClientRequestMetadata, 'receivedAt'> {
  readonly receivedAt?: string;
}

export interface KaanaRequestInput {
  readonly attribution: KaanaAttributionInput;
  readonly target: RoutingTarget;
  /** Defaults to `text`. */
  readonly modality?: InferenceModality;
  readonly input: KaanaInputInput;
  /**
   * Whether Kaana streams from the UPSTREAM provider. It does not decide how
   * Kaana answers Oxy, which is an event stream either way.
   */
  readonly stream: boolean;
  readonly maxOutputTokens?: number;
  readonly sampling?: SamplingParameters;
  readonly tools?: readonly ToolDefinition[];
  readonly toolChoice?: ToolChoice;
  readonly responseFormat?: ResponseFormat;
  readonly client: KaanaClientMetadataInput;
  readonly idempotencyKey?: string;
  readonly routingPolicy: RoutingPolicyReference;
}

function toMessage(message: KaanaMessageInput): InferenceMessage {
  return {
    ...message,
    content:
      typeof message.content === 'string'
        ? [{ type: 'text', text: message.content }]
        : [...message.content],
  };
}

function toInput(input: KaanaInputInput): InferenceRequestInput['input'] {
  if ('messages' in input) {
    return { format: 'messages', messages: input.messages.map(toMessage) };
  }
  if ('text' in input) {
    return { format: 'text', text: input.text };
  }
  return { format: 'text_batch', texts: [...input.texts] };
}

/**
 * Assembles and validates an inference request envelope.
 *
 * Validation is the published schema's own, so this cannot produce an envelope
 * the control plane's parse would reject. It throws {@link KaanaEnvelopeError}
 * with the ZodError as `cause`: the failing field path is the useful part, and
 * re-wording it here would create a second version of the message that drifts.
 */
export function buildInferenceRequest(input: KaanaRequestInput): InferenceRequest {
  // Typed as the schema's own input so a misspelled field is a compile error
  // rather than something only the runtime parse below notices.
  const candidate: InferenceRequestInput = {
    schemaVersion: 1,
    attribution: {
      principal: {
        billing: { accountId: input.attribution.accountId },
        applicationId: input.attribution.applicationId,
        credentialId: input.attribution.credentialId,
        environment: input.attribution.environment,
        inferenceScopes: [...(input.attribution.inferenceScopes ?? ['inference:invoke'])],
      },
      userId: input.attribution.userId,
      requestId: input.attribution.requestId,
      generationId: input.attribution.generationId,
    },
    target: input.target,
    modality: input.modality ?? 'text',
    input: toInput(input.input),
    stream: input.stream,
    maxOutputTokens: input.maxOutputTokens,
    sampling: input.sampling ?? {},
    tools: [...(input.tools ?? [])],
    toolChoice: input.toolChoice,
    responseFormat: input.responseFormat,
    client: {
      ...input.client,
      // The contract's canonical spelling: UTC, millisecond precision, trailing
      // `Z`, which is exactly what `toISOString` produces and what
      // `contract.NewTimestamp` renders on the other side.
      receivedAt: input.client.receivedAt ?? new Date().toISOString(),
    },
    idempotencyKey: input.idempotencyKey,
    routingPolicy: input.routingPolicy,
  };

  const parsed = inferenceRequestSchema.safeParse(candidate);
  if (!parsed.success) {
    throw new KaanaEnvelopeError('the inference envelope is not valid against the contract', {
      cause: parsed.error,
    });
  }
  return parsed.data;
}

/**
 * Renders an envelope to the exact bytes that will be signed AND sent.
 *
 * One buffer for both is the whole point. `edgeauth.Verify` hashes the body it
 * is about to parse, so signing one serialisation and transmitting another — a
 * second `JSON.stringify`, or a body the runtime re-encodes because it was
 * handed an object — authenticates something other than what Kaana executes,
 * and surfaces as a blanket `authentication_failed` that names nothing.
 */
export function serializeInferenceRequest(request: InferenceRequest): Uint8Array {
  return new TextEncoder().encode(JSON.stringify(request));
}
