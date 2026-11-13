import { describe, expect, it } from 'bun:test';
import { kaanaIdentityAssertionSchema, kaanaIdentityRequestSchema } from '../internal.js';

const assertion = {
  schemaVersion: 1,
  kind: 'identity',
  audience: 'kaana.ai',
  requestId: 'req_identity',
  requestSha256: 'a'.repeat(64),
  issuedAt: '2026-11-13T12:00:00Z',
  expiresAt: '2026-11-13T12:01:00Z',
  principal: {
    ownerAccountId: 'owner',
    applicationId: 'application',
    credentialId: 'workload',
    environment: 'production',
    lane: 'service_token',
    authority: 'first_party',
    applicationClass: 'internal',
    applicationIsInternal: true,
  },
};

describe('identity is independent of economic authorization', () => {
  it('requires explicit internal classification and the same bounded identities as Go', () => {
    const { applicationIsInternal: _flag, ...withoutFlag } = assertion.principal;
    for (const principal of [
      withoutFlag,
      { ...assertion.principal, applicationIsInternal: null },
      { ...assertion.principal, applicationId: 'a'.repeat(65) },
      { ...assertion.principal, applicationId: '😀'.repeat(33) },
    ]) {
      expect(kaanaIdentityAssertionSchema.safeParse({ ...assertion, principal }).success).toBe(
        false,
      );
    }
    expect(
      kaanaIdentityAssertionSchema.safeParse({
        ...assertion,
        principal: { ...assertion.principal, applicationId: '😀'.repeat(32) },
      }).success,
    ).toBe(true);
    for (const value of [
      { ...assertion, requestId: 'a'.repeat(129) },
      { ...assertion, userId: null },
      { ...assertion, userId: 'u'.repeat(65) },
      { ...assertion, issuedAt: '2026-11-13T12:00:00+00:00' },
    ]) {
      expect(kaanaIdentityAssertionSchema.safeParse(value).success).toBe(false);
    }
  });
  it('carries a live workload without fabricated scopes and external credentials with scopes', () => {
    expect(kaanaIdentityAssertionSchema.safeParse(assertion).success).toBe(true);
    expect(
      kaanaIdentityAssertionSchema.safeParse({
        ...assertion,
        principal: {
          ...assertion.principal,
          authority: 'external',
          lane: 'machine_credential',
          scopes: [],
        },
      }).success,
    ).toBe(true);
  });
  it('does not accept economic or inference fields, forged authority, sessions or unbounded validity', () => {
    for (const value of [
      { ...assertion, economics: { treatment: 'internal_metered' } },
      { ...assertion, expiresAt: '2026-11-13T12:01:01Z' },
      { ...assertion, expiresAt: assertion.issuedAt },
      { ...assertion, principal: { ...assertion.principal, scopes: [] } },
      { ...assertion, principal: { ...assertion.principal, lane: 'machine_credential' } },
      { ...assertion, principal: { ...assertion.principal, lane: 'product_session' } },
      { ...assertion, principal: { ...assertion.principal, authority: 'external' } },
      { ...assertion, principal: { ...assertion.principal, billingAccountId: 'another-owner' } },
    ])
      expect(kaanaIdentityAssertionSchema.safeParse(value).success).toBe(false);
    const request = {
      schemaVersion: 1,
      kind: 'identity',
      requestId: 'req_identity',
      audience: 'kaana.ai',
      credential: 'opaque-test-credential',
      onBehalfOfUserId: 'attribution',
    };
    expect(kaanaIdentityRequestSchema.safeParse(request).success).toBe(true);
    for (const field of [
      'model',
      'facts',
      'bodySha256',
      'economics',
      'reservation',
      'routingPolicy',
    ]) {
      expect(kaanaIdentityRequestSchema.safeParse({ ...request, [field]: {} }).success).toBe(false);
    }
  });
});
