/**
 * Wire fixtures, shaped like what `internal/relay` and `internal/httpapi`
 * actually emit.
 *
 * Each is PARSED through the published schema as it is defined, so a fixture
 * that has drifted from the contract fails at import rather than quietly proving
 * the client handles a shape nothing sends.
 *
 * Optional fields are populated on purpose: a field that drifted is invisible in
 * a minimal fixture, which is the same reason `internal/contract/fixtures_test.go`
 * writes a `messages-with-every-optional-field` case.
 */

import {
  inferenceStreamEventSchema,
  normalizedUsageReportSchema,
  type InferenceRequest,
  type InferenceStreamEvent,
  type NormalizedUsageReport,
} from '@oxyhq/contracts';

import { buildInferenceRequest } from '../src/envelope.js';

export const REQUEST_ID = 'req_01JQZABCDEF';
export const GENERATION_ID = 'gen_01JQZABCDEF';
export const MODEL_REFERENCE = 'openai/gpt-4o-mini';
export const SERVING_PROVIDER = 'openrouter';

/** A minimal chat envelope: what a `/v1/chat/completions` turn normalizes to. */
export function chatEnvelope(overrides: { readonly stream?: boolean } = {}): InferenceRequest {
  return buildInferenceRequest({
    attribution: {
      accountId: 'acc_01JQZ',
      applicationId: 'app_01JQZ',
      credentialId: 'cred_01JQZ',
      environment: 'production',
      requestId: REQUEST_ID,
    },
    target: { kind: 'model', modelReference: MODEL_REFERENCE },
    input: { messages: [{ role: 'user', content: 'hi' }] },
    stream: overrides.stream ?? true,
    maxOutputTokens: 16,
    sampling: { temperature: 0 },
    client: {
      apiFormat: 'chat_completions',
      endpoint: '/v1/chat/completions',
      receivedAt: '2026-08-24T00:00:00.000Z',
    },
    routingPolicy: { routingPolicyId: 'rp_01JQZ', policyVersion: 3 },
  });
}

export const START_EVENT: InferenceStreamEvent = inferenceStreamEventSchema.parse({
  schemaVersion: 1,
  type: 'start',
  requestId: REQUEST_ID,
  sequence: 0,
  generationId: GENERATION_ID,
  resolvedModelReference: MODEL_REFERENCE,
  servingProvider: SERVING_PROVIDER,
  startedAt: '2026-08-24T00:00:00.000Z',
});

export function deltaEvent(sequence: number, text: string): InferenceStreamEvent {
  return inferenceStreamEventSchema.parse({
    schemaVersion: 1,
    type: 'delta',
    requestId: REQUEST_ID,
    sequence,
    outputIndex: 0,
    channel: 'output_text',
    text,
  });
}

export const REASONING_EVENT: InferenceStreamEvent = inferenceStreamEventSchema.parse({
  schemaVersion: 1,
  type: 'delta',
  requestId: REQUEST_ID,
  sequence: 2,
  outputIndex: 0,
  channel: 'reasoning',
  text: 'thinking',
});

/**
 * A deployment-scope switch: the same model reference, served somewhere else.
 * `internal/relay` cannot construct any other kind from one `RouteSet`.
 */
export const ROUTE_SWITCH_EVENT: InferenceStreamEvent = inferenceStreamEventSchema.parse({
  schemaVersion: 1,
  type: 'route_switch',
  requestId: REQUEST_ID,
  sequence: 1,
  reason: 'provider_overloaded',
  detail: {
    scope: 'deployment',
    modelReference: MODEL_REFERENCE,
    toProvider: SERVING_PROVIDER,
    toDeploymentId: 'dep_02JQZ',
  },
  occurredAt: '2026-08-24T00:00:00.500Z',
});

export const TOOL_CALL_EVENTS: readonly InferenceStreamEvent[] = [
  inferenceStreamEventSchema.parse({
    schemaVersion: 1,
    type: 'tool_call',
    requestId: REQUEST_ID,
    sequence: 5,
    toolCallId: 'call_1',
    name: 'lookup',
    argumentsDelta: '{"q":',
    complete: false,
  }),
  inferenceStreamEventSchema.parse({
    schemaVersion: 1,
    type: 'tool_call',
    requestId: REQUEST_ID,
    sequence: 6,
    toolCallId: 'call_1',
    argumentsDelta: '"x"}',
    complete: true,
  }),
];

export const USAGE_EVENT: InferenceStreamEvent = inferenceStreamEventSchema.parse({
  schemaVersion: 1,
  type: 'usage',
  requestId: REQUEST_ID,
  sequence: 7,
  units: [
    { unit: 'input_tokens', quantity: 12 },
    { unit: 'output_tokens', quantity: 2 },
  ],
  usageSource: 'provider_reported',
});

export const DONE_EVENT: InferenceStreamEvent = inferenceStreamEventSchema.parse({
  schemaVersion: 1,
  type: 'done',
  requestId: REQUEST_ID,
  sequence: 8,
  generationId: GENERATION_ID,
  finishReason: 'stop',
  completedAt: '2026-08-24T00:00:01.000Z',
});

/** `provider_overloaded` is retryable; the contract constrains that by the code. */
export const ERROR_EVENT: InferenceStreamEvent = inferenceStreamEventSchema.parse({
  schemaVersion: 1,
  type: 'error',
  requestId: REQUEST_ID,
  sequence: 2,
  error: {
    schemaVersion: 1,
    code: 'provider_overloaded',
    message: 'the upstream is overloaded',
    retryable: true,
    requestId: REQUEST_ID,
    retryAfterMs: 1500,
    upstreamCategory: 'overloaded',
    providerError: { provider: SERVING_PROVIDER, status: 529 },
  },
});

export const USAGE_REPORT: NormalizedUsageReport = normalizedUsageReportSchema.parse({
  schemaVersion: 1,
  requestId: REQUEST_ID,
  generationId: GENERATION_ID,
  attribution: {
    principal: {
      billing: { accountId: 'acc_01JQZ' },
      applicationId: 'app_01JQZ',
      credentialId: 'cred_01JQZ',
      environment: 'production',
      inferenceScopes: ['inference:invoke'],
    },
    userId: 'usr_01JQZ',
    requestId: REQUEST_ID,
  },
  outcome: 'completed',
  units: [
    { unit: 'input_tokens', quantity: 12 },
    { unit: 'output_tokens', quantity: 2 },
  ],
  usageSource: 'provider_reported',
  resolvedModelReference: MODEL_REFERENCE,
  servingProvider: SERVING_PROVIDER,
  deploymentId: 'dep_01JQZ',
  routeSwitches: 0,
  startedAt: '2026-08-24T00:00:00.000Z',
  completedAt: '2026-08-24T00:00:01.000Z',
  timeToFirstTokenMs: 240,
});

/** Renders frames exactly as `internal/sse`'s writer does. */
export function sseWire(
  frames: readonly { readonly name: string; readonly payload: unknown }[],
): string {
  return frames
    .map(({ name, payload }) => `event: ${name}\ndata: ${JSON.stringify(payload)}\n\n`)
    .join('');
}

/** The health projection, as `internal/httpapi` renders it. */
export const HEALTH_RESPONSE = {
  contractVersion: '1.1.0',
  checkedAt: '2026-08-24T00:00:00.000Z',
  providers: [
    {
      provider: SERVING_PROVIDER,
      status: 'ok',
      checkedAt: '2026-08-24T00:00:00.000Z',
      latencyMs: 42,
      detail: 'the probe answered',
      credentials: {
        declared: 2,
        usable: 1,
        keys: [
          { position: 0, state: 'usable' },
          { position: 1, state: 'exhausted', retiredUntil: '2026-08-24T01:00:00.000Z' },
        ],
      },
    },
  ],
  configuration: {
    snapshotId: 'snap_01JQZ',
    issuedAt: '2026-08-24T00:00:00.000Z',
    ageSeconds: 30,
    maxAgeSeconds: 3600,
    servesUnpinnedReferences: true,
  },
  deployments: [
    {
      deploymentId: 'dep_01JQZ',
      state: 'closed',
      score: 1,
      consecutiveFailures: 0,
      provider: SERVING_PROVIDER,
    },
    {
      deploymentId: 'dep_02JQZ',
      state: 'open',
      score: 0,
      consecutiveFailures: 4,
      probesAt: '2026-08-24T00:01:00.000Z',
      provider: SERVING_PROVIDER,
    },
  ],
} as const;
