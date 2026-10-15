import { describe, expect, it } from 'bun:test';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  authorizationRequestSchema,
  canonicalInputSha256,
  canonicalJson,
  GRANT_SIGNATURE_DOMAIN,
  grantSchema,
  SETTLEMENT_SIGNATURE_DOMAIN,
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

  it('binds an approval-bound operation to one exact route at its admitted price', () => {
    const exact = {
      ...commercialGrant,
      steps: [
        {
          target: { kind: 'model', modelLine: 'openai/gpt-5', revision: '2026-06-01' },
          limits: commercialGrant.steps[0].limits,
        },
      ],
      reservation: { ...commercialGrant.reservation, fundingRestriction: 'promotional-only' },
      exactRoute: {
        approval: 'scoped_execution',
        deploymentId: 'dep_openai_gpt5_use1',
        provider: 'openai',
        keyId: 'key_scoped_1',
        modelReference: 'openai/gpt-5@2026-06-01',
        priceVersionId: 'pv_1',
        inputSha256: 'b'.repeat(64),
      },
    };
    expect(grantSchema.safeParse(exact).success).toBe(true);
    expect(refused({ ...exact, steps: [...exact.steps, ...exact.steps] })).toBe(true);
    expect(
      refused({ ...exact, exactRoute: { ...exact.exactRoute, priceVersionId: 'pv_other' } }),
    ).toBe(true);
    expect(
      refused({
        ...exact,
        exactRoute: { ...exact.exactRoute, modelReference: 'openai/gpt-5@2026-07-01' },
      }),
    ).toBe(true);
  });

  it('refuses a field the grant does not define', () => {
    expect(refused({ ...commercialGrant, prompt: 'never' })).toBe(true);
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

describe('signing and the canonical input hash', () => {
  it('signs the scheme every Oxy↔Kaana body already uses, under its own domains', () => {
    expect(GRANT_SIGNATURE_DOMAIN).toBe('oxy-kaana-grant:v1');
    expect(SETTLEMENT_SIGNATURE_DOMAIN).toBe('kaana-oxy-settlement:v1');
    expect(
      signatureInput(GRANT_SIGNATURE_DOMAIN, 'oxy-2026-10', 1760521260000, 'd'.repeat(64)),
    ).toBe(`oxy-kaana-grant:v1\noxy-2026-10\n1760521260000\n${'d'.repeat(64)}`);
    expect(() => signatureInput(GRANT_SIGNATURE_DOMAIN, 'k', 1, 'D'.repeat(64))).toThrow();
  });

  it('hashes the canonical JSON of the input, as Oxy and Go compute it', async () => {
    const input = { format: 'text', text: 'Hola 👋', z: [1, 2], a: { b: true } };
    const expected = createHash('sha256').update(canonicalJson(input), 'utf8').digest('hex');
    expect(await canonicalInputSha256(input)).toBe(expected);
    expect(canonicalJson(input)).toBe(
      '{"a":{"b":true},"format":"text","text":"Hola 👋","z":[1,2]}',
    );
  });

  it('reproduces the frozen cross-language vectors', async () => {
    const golden = JSON.parse(
      readFileSync(join(import.meta.dir, 'moved', 'scopedExecution.golden.json'), 'utf8'),
    ) as { canonicalInputs: { input: unknown; canonical: string; sha256: string }[] };
    expect(golden.canonicalInputs.length).toBeGreaterThan(0);
    for (const vector of golden.canonicalInputs) {
      expect(canonicalJson(vector.input)).toBe(vector.canonical);
      expect(await canonicalInputSha256(vector.input)).toBe(vector.sha256);
    }
  });
});
