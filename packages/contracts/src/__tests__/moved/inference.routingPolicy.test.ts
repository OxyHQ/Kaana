import { describe, expect, it } from 'bun:test';
import { routingPolicyReferenceSchema, safeParseContract } from './contract.js';

describe('routingPolicyReferenceSchema', () => {
  it('records exactly which policy revision served a request', () => {
    expect(
      safeParseContract(routingPolicyReferenceSchema, {
        routingPolicyId: 'rp_1',
        policyVersion: 7,
      }),
    ).toEqual({ routingPolicyId: 'rp_1', policyVersion: 7 });
  });

  it('refuses an unversioned reference', () => {
    expect(routingPolicyReferenceSchema.safeParse({ routingPolicyId: 'rp_1' }).success).toBe(false);
  });
});
