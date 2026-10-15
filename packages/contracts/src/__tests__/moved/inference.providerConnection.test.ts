import { describe, expect, it } from 'bun:test';
import {
  authorizedRouteSchema,
  kaanaCredentialMutationSchema,
  kaanaCredentialOutcomeRequestSchema,
  kaanaCredentialOutcomeSchema,
  kaanaCredentialHandleSchema,
  kaanaCredentialValidationOutcomeSchema,
} from './contract.js';

const handle = `kcred_${'a'.repeat(26)}`;
describe('Kaana provider credential contracts', () => {
  it('accepts only opaque Kaana handles', () => {
    expect(kaanaCredentialHandleSchema.safeParse(handle).success).toBe(true);
    for (const invalid of [
      'vault:oxy/inference/byok/production/acc_1/pcx_1',
      'ssm:/customer/key',
      'sk-live-credential',
      `kcred_${'A'.repeat(26)}`,
    ]) {
      expect(kaanaCredentialHandleSchema.safeParse(invalid).success).toBe(false);
    }
  });

  it('binds mutations to one exact operation, identity, actor and action shape', () => {
    const create = {
      schemaVersion: 1,
      operationId: 'op_create_1',
      action: 'create',
      provider: 'openai',
      ownerAccountId: 'acc_1',
      connectionId: 'pcx_1',
      environment: 'production',
      operationActor: 'user:user_1',
      secretBase64: Buffer.from('customer-provider-key').toString('base64'),
    };
    expect(kaanaCredentialMutationSchema.safeParse(create).success).toBe(true);
    expect(
      kaanaCredentialMutationSchema.safeParse({ ...create, credentialHandle: handle }).success,
    ).toBe(false);
    expect(
      kaanaCredentialMutationSchema.safeParse({ ...create, ownerAccountId: 'acc/guessed' }).success,
    ).toBe(false);
    expect(kaanaCredentialMutationSchema.safeParse({ ...create, actor: {} }).success).toBe(false);

    for (const invalidSecret of [
      Buffer.from([0]),
      Buffer.from(' customer-provider-key '),
      Buffer.from('customer\tprovider'),
      Buffer.from('credencial-ñ'),
      Buffer.from('a'.repeat(4097)),
    ]) {
      expect(
        kaanaCredentialMutationSchema.safeParse({
          ...create,
          secretBase64: invalidSecret.toString('base64'),
        }).success,
      ).toBe(false);
    }
    expect(
      kaanaCredentialMutationSchema.safeParse({
        ...create,
        secretBase64: Buffer.from('a'.repeat(4096)).toString('base64'),
      }).success,
    ).toBe(true);
    expect(
      kaanaCredentialMutationSchema.safeParse({ ...create, secretBase64: 'YR==' }).success,
    ).toBe(false);
    expect(
      kaanaCredentialMutationSchema.safeParse({ ...create, secretBase64: 'YWJ=' }).success,
    ).toBe(false);
    expect(
      kaanaCredentialMutationSchema.safeParse({ ...create, secretBase64: 'YQ==' }).success,
    ).toBe(true);
    expect(
      kaanaCredentialMutationSchema.safeParse({ ...create, secretBase64: 'YWI=' }).success,
    ).toBe(true);
  });

  it('keeps outcome queries metadata-only and conflicts reference-free', () => {
    const request = {
      schemaVersion: 1,
      operationId: 'op_rotate_1',
      action: 'rotate',
      provider: 'openai',
      ownerAccountId: 'acc_1',
      connectionId: 'pcx_1',
      environment: 'production',
      credentialHandle: handle,
      expectedRevision: 2,
    };
    expect(kaanaCredentialOutcomeRequestSchema.safeParse(request).success).toBe(true);
    expect(
      kaanaCredentialOutcomeRequestSchema.safeParse({ ...request, secretBase64: 'c2VjcmV0' })
        .success,
    ).toBe(false);
    expect(
      kaanaCredentialOutcomeSchema.safeParse({
        schemaVersion: 1,
        operationId: 'op_rotate_1',
        action: 'rotate',
        status: 'conflict',
      }).success,
    ).toBe(true);
    expect(
      kaanaCredentialOutcomeSchema.safeParse({
        schemaVersion: 1,
        operationId: 'op_rotate_1',
        action: 'rotate',
        status: 'conflict',
        credentialHandle: handle,
        revision: 3,
      }).success,
    ).toBe(false);
  });

  it('carries the full exact identity on a customer-authorized route', () => {
    const route = {
      substitution: 'same_model',
      deploymentId: 'dep_1',
      modelReference: 'openai/gpt@2026-09-01',
      provider: 'openai',
      regions: ['us-west-2'],
      customerProviderCredential: {
        credentialHandle: handle,
        credentialRevision: 2,
        ownerAccountId: 'acc_1',
        connectionId: 'pcx_1',
        environment: 'production',
      },
    };
    expect(authorizedRouteSchema.safeParse(route).success).toBe(true);
    expect(
      authorizedRouteSchema.safeParse({
        ...route,
        customerProviderCredential: {
          ...route.customerProviderCredential,
          credentialRevision: 0,
        },
      }).success,
    ).toBe(false);
  });

  it('separates exact internal bootstrap binding from the customer-safe operation', () => {
    const task = {
      schemaVersion: 1,
      operationId: 'operation_exact',
      applicationId: 'app_1',
      provider: 'openai',
      ownerAccountId: 'acc_1',
      connectionId: 'pcx_1',
      environment: 'production',
      credentialHandle: handle,
      credentialRevision: 2,
      deploymentId: 'kaana_deployment_exact',
    };
    expect(
      kaanaCredentialValidationOutcomeSchema.safeParse({
        ...task,
        state: 'inconclusive',
        failureCode: 'forbidden',
      }).success,
    ).toBe(true);
    expect(
      kaanaCredentialValidationOutcomeSchema.safeParse({
        ...task,
        state: 'invalid',
        failureCode: 'forbidden',
      }).success,
    ).toBe(false);
  });
});
