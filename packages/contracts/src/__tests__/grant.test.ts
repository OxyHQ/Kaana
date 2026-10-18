import { describe, expect, it } from 'bun:test';
import {
  authorizationRequestSchema,
  deploymentCommercialSchema,
  GRANT_SIGNATURE_DOMAIN,
  grantSchema,
  grantStepLimitsSchema,
  SETTLEMENT_READ_PATH,
  SETTLEMENT_READ_SIGNATURE_DOMAIN,
  SETTLEMENT_SIGNATURE_DOMAIN,
  settlementReadQuery,
  settlementSchema,
  signatureInput,
} from '../internal.js';
import { aliaProGrant, commercialGrant, requestId, settlement, usageReport } from './fixtures.js';

const refused = (value: unknown) => grantSchema.safeParse(value).success === false;

describe('the grant carries today’s funding model', () => {
  it('parses a commercial grant and the two-step Alia Pro lane', () => {
    expect(grantSchema.safeParse(commercialGrant).success).toBe(true);
    const pro = grantSchema.parse(aliaProGrant);
    expect(pro.steps.map((step) => step.target)).toEqual([
      { kind: 'level', level: 'pro' },
      { kind: 'level', level: 'xhigh' },
    ]);
    expect(pro.steps[0].limits.maxFundingTier).toBe('credit');
  });

  it('holds nothing for an internally metered grant, and always holds for a charged one', () => {
    expect(
      refused({
        ...aliaProGrant,
        reservation: { reservationId: 'res_1', ceiling: { amount: '1.00', currency: 'USD' } },
      }),
    ).toBe(true);
    expect(refused({ ...commercialGrant, reservation: undefined })).toBe(true);
  });

  it('is BYOK exactly when the treatment is platform_fee_only, and then has no funding ceiling', () => {
    const byok = {
      credentialHandle: `kcred_${'a'.repeat(26)}`,
      credentialRevision: 2,
      ownerAccountId: 'acc_1',
      connectionId: 'pcx_1',
      environment: 'production',
    };
    const platformFee = {
      ...commercialGrant,
      economics: { ...commercialGrant.economics, treatment: 'platform_fee_only' },
      customerProviderCredential: byok,
    };
    expect(grantSchema.safeParse(platformFee).success).toBe(true);
    expect(refused({ ...commercialGrant, customerProviderCredential: byok })).toBe(true);
    expect(refused({ ...platformFee, customerProviderCredential: undefined })).toBe(true);
    expect(
      refused({
        ...platformFee,
        steps: [
          {
            ...commercialGrant.steps[0],
            limits: { ...commercialGrant.steps[0].limits, maxFundingTier: 'credit' },
          },
        ],
      }),
    ).toBe(true);
  });

  it('is executable for at most 60 seconds', () => {
    expect(refused({ ...commercialGrant, executeBy: '2026-10-15T09:42:00.001Z' })).toBe(true);
    expect(refused({ ...commercialGrant, executeBy: commercialGrant.issuedAt })).toBe(true);
  });

  it('admits the model line a model step names', () => {
    expect(
      refused({
        ...commercialGrant,
        steps: [
          {
            target: { kind: 'model', modelLine: 'anthropic/claude-opus-5' },
            limits: commercialGrant.steps[0].limits,
          },
        ],
      }),
    ).toBe(true);
  });

  it('refuses a field the grant does not define', () => {
    expect(refused({ ...commercialGrant, prompt: 'never' })).toBe(true);
  });
});

describe('sameModelFailover (contract set 4.4.0)', () => {
  const limits = commercialGrant.steps[0].limits;
  const { sameModelFailover: _omitted, ...withoutFlag } = limits as typeof limits & {
    sameModelFailover?: boolean;
  };

  it('defaults to true when absent, which is what 4.3.0 did', () => {
    expect(grantStepLimitsSchema.parse(withoutFlag).sameModelFailover).toBe(true);
  });

  it('carries false, and refuses anything that is not a boolean', () => {
    expect(
      grantStepLimitsSchema.parse({ ...withoutFlag, sameModelFailover: false }).sameModelFailover,
    ).toBe(false);
    expect(
      grantStepLimitsSchema.safeParse({ ...withoutFlag, sameModelFailover: 'no' }).success,
    ).toBe(false);
  });
});

describe('deploymentCommercialSchema (contract set 4.4.0)', () => {
  const sold = {
    availabilityScope: 'platform_internal',
    commercialPermission: 'standard_application_use',
  };

  it('parses a sold deployment, with and without dedicatedCapacity', () => {
    expect(deploymentCommercialSchema.safeParse(sold).success).toBe(true);
    expect(
      deploymentCommercialSchema.safeParse({
        availabilityScope: 'enterprise',
        commercialPermission: 'wholesale_contract',
        dedicatedCapacity: true,
      }).success,
    ).toBe(true);
  });

  it('is strict and names only the contract’s scopes and permissions', () => {
    expect(deploymentCommercialSchema.safeParse({ ...sold, status: 'active' }).success).toBe(false);
    expect(
      deploymentCommercialSchema.safeParse({ ...sold, availabilityScope: 'internal_alia' }).success,
    ).toBe(false);
    expect(
      deploymentCommercialSchema.safeParse({ ...sold, commercialPermission: 'free' }).success,
    ).toBe(false);
    expect(
      deploymentCommercialSchema.safeParse({ ...sold, dedicatedCapacity: 'yes' }).success,
    ).toBe(false);
  });

  it('pairs customer_byok with byok_only, both ways', () => {
    expect(
      deploymentCommercialSchema.safeParse({
        availabilityScope: 'byok_only',
        commercialPermission: 'customer_byok',
      }).success,
    ).toBe(true);
    expect(
      deploymentCommercialSchema.safeParse({ ...sold, commercialPermission: 'customer_byok' })
        .success,
    ).toBe(false);
    expect(
      deploymentCommercialSchema.safeParse({ ...sold, availabilityScope: 'byok_only' }).success,
    ).toBe(false);
  });

  it('requires an approved resale permission for public pay-as-you-go', () => {
    expect(
      deploymentCommercialSchema.safeParse({ ...sold, availabilityScope: 'public_payg' }).success,
    ).toBe(false);
    expect(
      deploymentCommercialSchema.safeParse({
        availabilityScope: 'public_payg',
        commercialPermission: 'public_resale_approved',
      }).success,
    ).toBe(true);
  });
});

describe('the authorization request carries facts, never content', () => {
  const inference = {
    schemaVersion: 1,
    kind: 'inference',
    credential: 'oxy_sk_test_opaque',
    requestId,
    operation: 'audio_speech',
    target: { kind: 'model', modelLine: 'elevenlabs/eleven-multilingual' },
    facts: { inputCharacters: 4, declaredTools: 0, stream: true },
    bodySha256: 'c'.repeat(64),
    client: { endpoint: '/v1/audio/speech', receivedAt: '2026-10-15T09:41:00.000Z' },
  };

  it('parses an inference, a catalogue and a receipt authorization', () => {
    expect(authorizationRequestSchema.safeParse(inference).success).toBe(true);
    expect(
      authorizationRequestSchema.safeParse({
        schemaVersion: 1,
        kind: 'catalogue.read',
        credential: 'oxy_sk_test_opaque',
        requestId,
      }).success,
    ).toBe(true);
    expect(
      authorizationRequestSchema.safeParse({
        schemaVersion: 1,
        kind: 'receipt.read',
        credential: 'oxy_sk_test_opaque',
        requestId,
        idempotencyKey: 'idem_1',
      }).success,
    ).toBe(true);
  });

  it('refuses the prompt, or any field it does not define', () => {
    expect(authorizationRequestSchema.safeParse({ ...inference, input: 'Hola' }).success).toBe(
      false,
    );
    expect(
      authorizationRequestSchema.safeParse({
        ...inference,
        facts: { ...inference.facts, text: 'Hola' },
      }).success,
    ).toBe(false);
  });

  it('reads a receipt by exactly one key', () => {
    const receipt = {
      schemaVersion: 1,
      kind: 'receipt.read',
      credential: 'oxy_sk_test_opaque',
      requestId,
    };
    expect(authorizationRequestSchema.safeParse(receipt).success).toBe(false);
    expect(
      authorizationRequestSchema.safeParse({
        ...receipt,
        generationId: 'gen_1',
        idempotencyKey: 'idem_1',
      }).success,
    ).toBe(false);
  });
});

describe('settlement', () => {
  it('settles the request its report measured', () => {
    expect(settlementSchema.safeParse(settlement).success).toBe(true);
    expect(
      settlementSchema.safeParse({ ...settlement, report: { ...usageReport, requestId: 'other' } })
        .success,
    ).toBe(false);
  });

  it('names the step that served a completed request', () => {
    expect(settlementSchema.safeParse({ ...settlement, servedStep: undefined }).success).toBe(
      false,
    );
  });
});

describe('signing', () => {
  it('signs the scheme every Oxy↔Kaana body already uses, under its own domains', () => {
    expect(GRANT_SIGNATURE_DOMAIN).toBe('oxy-kaana-grant:v1');
    expect(SETTLEMENT_SIGNATURE_DOMAIN).toBe('kaana-oxy-settlement:v1');
    expect(SETTLEMENT_READ_SIGNATURE_DOMAIN).toBe('oxy-kaana-settlement-read:v1');
    expect(
      signatureInput(GRANT_SIGNATURE_DOMAIN, 'oxy-2026-10', 1760521260000, 'd'.repeat(64)),
    ).toBe(`oxy-kaana-grant:v1\noxy-2026-10\n1760521260000\n${'d'.repeat(64)}`);
    expect(() => signatureInput(GRANT_SIGNATURE_DOMAIN, 'k', 1, 'D'.repeat(64))).toThrow();
  });

  it('signs a reconciliation read over its raw query, never its path', () => {
    expect(SETTLEMENT_READ_PATH).toBe('/internal/v1/settlements');
    expect(settlementReadQuery(null)).toBe('');
    expect(settlementReadQuery('42')).toBe('cursor=42');
    expect(settlementReadQuery('a b/c')).toBe('cursor=a%20b%2Fc');
  });
});
