import { describe, expect, it } from 'bun:test';
import * as publicContract from '../index.js';
import * as internalContract from '../internal.js';
import { publicError, requestId } from './fixtures.js';

const isSchema = (value: unknown): value is { _zod: { def: Record<string, unknown> } } =>
  typeof value === 'object' && value !== null && '_zod' in value;

/** Every object field name reachable from a schema, through every wrapper. */
function fieldNames(schema: unknown, seen = new Set<unknown>()): Set<string> {
  const names = new Set<string>();
  const visit = (node: unknown): void => {
    if (!isSchema(node) || seen.has(node)) return;
    seen.add(node);
    const def = node._zod.def as Record<string, unknown>;
    if (def.type === 'object') {
      for (const [name, field] of Object.entries(def.shape as Record<string, unknown>)) {
        names.add(name);
        visit(field);
      }
    }
    for (const key of ['innerType', 'element', 'keyType', 'valueType', 'in', 'out']) {
      visit(def[key]);
    }
    for (const option of (def.options as unknown[] | undefined) ?? []) visit(option);
  };
  visit(schema);
  return names;
}

/** Every string an enum or literal can take, reachable from a schema. */
function enumValues(schema: unknown, seen = new Set<unknown>()): Set<unknown> {
  const values = new Set<unknown>();
  const visit = (node: unknown): void => {
    if (!isSchema(node) || seen.has(node)) return;
    seen.add(node);
    const def = node._zod.def as Record<string, unknown>;
    if (def.type === 'enum') for (const v of Object.values(def.entries as object)) values.add(v);
    if (def.type === 'literal') for (const v of def.values as unknown[]) values.add(v);
    if (def.type === 'object') for (const field of Object.values(def.shape as object)) visit(field);
    for (const key of ['innerType', 'element', 'keyType', 'valueType']) visit(def[key]);
    for (const option of (def.options as unknown[] | undefined) ?? []) visit(option);
  };
  visit(schema);
  return values;
}

const publicSchemas = Object.entries(publicContract).filter(([, value]) => isSchema(value));

describe('the public surface names no provider, operator, key or deployment', () => {
  const forbidden = [
    'provider',
    'servingProvider',
    'toProvider',
    'deploymentId',
    'toDeploymentId',
    'providerError',
    'keyId',
    'credentialHandle',
    'upstreamModelId',
    'fundingTier',
    'maxFundingTier',
    'billingAccountId',
    'authorizedRoutes',
  ];

  it.each(publicSchemas.map(([name, schema]) => [name, schema] as const))(
    '%s carries none of them',
    (_name, schema) => {
      const names = fieldNames(schema);
      expect(forbidden.filter((field) => names.has(field))).toEqual([]);
    },
  );

  it('the walk itself finds a provider where there is one (positive control)', () => {
    expect(fieldNames(internalContract.normalizedUsageReportSchema).has('servingProvider')).toBe(
      true,
    );
    expect(fieldNames(internalContract.inferenceErrorSchema).has('providerError')).toBe(true);
    expect(fieldNames(internalContract.realtimeServerEventSchema).has('deploymentId')).toBe(true);
  });

  it('no public enum can carry an internal-only error code', () => {
    for (const [, schema] of publicSchemas) {
      const values = enumValues(schema);
      for (const code of Object.keys(internalContract.INTERNAL_ONLY_ERROR_CODES)) {
        expect(values.has(code)).toBe(false);
      }
    }
  });
});

describe('the two entry points', () => {
  it('keeps every internal shape out of the package root', () => {
    for (const name of [
      'grantSchema',
      'settlementSchema',
      'inferenceErrorSchema',
      'INFERENCE_ERROR_CODES',
      'inferenceRequestSchema',
      'normalizedUsageReportSchema',
      'deploymentIdSchema',
      'inferenceProviderSlugSchema',
      'modelDeploymentSchema',
      'realtimeServerEventSchema',
      'canonicalInputSha256',
    ]) {
      expect(name in publicContract).toBe(false);
      expect(name in internalContract).toBe(true);
    }
  });

  it('does not repeat a public export under /internal', () => {
    const overlap = Object.keys(internalContract).filter((name) => name in publicContract);
    expect(overlap).toEqual([]);
  });

  it('leaves what stays in @oxy.so/contracts out of both', () => {
    for (const name of [
      'routingPolicySchema',
      'priceVersionSchema',
      'usageReservationSchema',
      'usageReceiptSchema',
      'usageRefundSchema',
      'accountBillingStateSchema',
      'productEntitlementSchema',
      'inboxComposeRequestSchema',
      'aliaModelReleaseManifestSchema',
      'modelDocumentationSchema',
      'providerConnectionSchema',
      'costCenterUsageSchema',
      'meteredGenerationSchema',
    ]) {
      expect(name in publicContract || name in internalContract).toBe(false);
    }
  });
});

describe('public errors', () => {
  const {
    PUBLIC_ERROR_CODES,
    NON_RETRYABLE_PUBLIC_ERROR_CODES,
    PUBLIC_ERROR_HTTP_STATUS,
    publicErrorSchema,
  } = publicContract;
  const { INFERENCE_ERROR_CODES, NON_RETRYABLE_INFERENCE_ERROR_CODES } = internalContract;
  const internalOnly = [
    'provider_credential_invalid',
    'provider_billing_refused',
    'deployment_unavailable',
  ];

  it('is the internal vocabulary minus exactly the three provider-side codes', () => {
    expect<string[]>([...PUBLIC_ERROR_CODES]).toEqual(
      INFERENCE_ERROR_CODES.filter((code) => !internalOnly.includes(code)),
    );
    expect(Object.keys(internalContract.INTERNAL_ONLY_ERROR_CODES).sort()).toEqual(
      [...internalOnly].sort(),
    );
  });

  it('keeps byok_credential_invalid: it is the customer’s own key', () => {
    expect(PUBLIC_ERROR_CODES).toContain('byok_credential_invalid');
  });

  it('keeps the internal retryability of every public code', () => {
    const internalNonRetryable = new Set<string>(NON_RETRYABLE_INFERENCE_ERROR_CODES);
    expect<string[]>([...NON_RETRYABLE_PUBLIC_ERROR_CODES]).toEqual(
      PUBLIC_ERROR_CODES.filter((code) => internalNonRetryable.has(code)),
    );
  });

  it('answers every public code with an HTTP status', () => {
    expect(Object.keys(PUBLIC_ERROR_HTTP_STATUS).sort()).toEqual([...PUBLIC_ERROR_CODES].sort());
  });

  it.each(internalOnly)('refuses the internal-only code %s on the public body', (code) => {
    expect(publicErrorSchema.safeParse({ ...publicError, code, retryable: false }).success).toBe(
      false,
    );
  });

  it('refuses a provider passthrough on the public body', () => {
    expect(
      publicErrorSchema.safeParse({
        ...publicError,
        providerError: { provider: 'openai', status: 429 },
      }).success,
    ).toBe(false);
  });

  it('holds the retryability rule', () => {
    expect(
      publicErrorSchema.safeParse({ ...publicError, code: 'invalid_request', retryable: true })
        .success,
    ).toBe(false);
    expect(
      publicErrorSchema.safeParse({ ...publicError, retryable: false, code: 'model_not_found' })
        .success,
    ).toBe(false);
  });

  it('refuses credential-shaped text in a message', () => {
    expect(
      publicErrorSchema.safeParse({
        ...publicError,
        message: 'upstream said: Authorization: Bearer abcdefghijklmnop',
      }).success,
    ).toBe(false);
  });
});

describe('projecting an internal error onto the public vocabulary', () => {
  const { toPublicError, toPublicErrorCode, inferenceErrorSchema } = internalContract;

  it.each([
    'provider_credential_invalid',
    'provider_billing_refused',
    'deployment_unavailable',
  ] as const)('%s becomes a retryable service_unavailable that names nobody', (code) => {
    const internal = inferenceErrorSchema.parse({
      schemaVersion: 1,
      code,
      message: 'openai refused key kp_7 for deployment dep_openai_use1',
      retryable: false,
      requestId,
      upstreamCategory: 'authentication',
      providerError: { provider: 'openai', status: 401, code: 'invalid_api_key' },
    });
    const projected = toPublicError(internal);
    expect(projected.code).toBe('service_unavailable');
    expect(projected.retryable).toBe(true);
    expect(projected).not.toHaveProperty('providerError');
    expect(projected).not.toHaveProperty('upstreamCategory');
    expect(projected.message).not.toContain('openai');
    expect(projected.message).not.toContain('dep_');
    expect(toPublicErrorCode(code)).toBe('service_unavailable');
  });

  it('keeps a public code and drops only the passthrough', () => {
    const internal = inferenceErrorSchema.parse({
      schemaVersion: 1,
      code: 'provider_overloaded',
      message: 'The model is overloaded. Retry shortly.',
      retryable: true,
      retryAfterMs: 2000,
      requestId,
      upstreamCategory: 'overloaded',
      providerError: { provider: 'anthropic', status: 529 },
    });
    expect(toPublicError(internal)).toEqual({
      schemaVersion: 1,
      code: 'provider_overloaded',
      message: 'The model is overloaded. Retry shortly.',
      retryable: true,
      retryAfterMs: 2000,
      requestId,
      upstreamCategory: 'overloaded',
    });
  });

  it('maps every internal code onto a public one', () => {
    const publicCodes = new Set<string>(publicContract.PUBLIC_ERROR_CODES);
    for (const code of internalContract.INFERENCE_ERROR_CODES) {
      expect(publicCodes.has(toPublicErrorCode(code))).toBe(true);
    }
  });
});
