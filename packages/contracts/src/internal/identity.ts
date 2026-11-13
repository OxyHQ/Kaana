/** Identity facts only. Kaana owns admission, routing and economic policy. */
import { z } from 'zod';
import { maxUtf16Length, minUtf16Length } from '../stringBounds.js';
import {
  delegatedUserIdSchema,
  inferenceEnvironmentSchema,
  inferenceTimestampSchema,
  oxyApplicationIdSchema,
  oxyCredentialIdSchema,
  requestIdSchema,
} from '../public/identifiers.js';
import { inferenceScopeSchema } from './attribution.js';
import { oxyAccountIdSchema } from './identifiers.js';

export const IDENTITY_SIGNATURE_DOMAIN = 'oxy-kaana-identity:v1';
export const IDENTITY_VALIDITY_WINDOW_MS = 60_000;
export const OXY_KAANA_IDENTITIES_ENDPOINT = '/internal/kaana/v1/identities';

/** The exact bytes of this request, not inference content, are signed by hash. */
export const kaanaIdentityRequestSchema = z
  .object({
    schemaVersion: z.literal(1),
    kind: z.literal('identity'),
    requestId: requestIdSchema,
    audience: z.literal('kaana.ai'),
    credential: z.string().check(minUtf16Length(1)).check(maxUtf16Length(8192)),
    /** Attribution only. Never proof of the person's identity or data authority. */
    onBehalfOfUserId: delegatedUserIdSchema.optional(),
  })
  .strict();

export const kaanaIdentityLaneSchema = z.enum([
  'machine_credential',
  'service_token',
  'product_session',
]);
export const kaanaIdentityApplicationClassSchema = z.enum([
  'first_party',
  'third_party',
  'internal',
  'system',
]);

const principalFields = {
  ownerAccountId: oxyAccountIdSchema,
  applicationId: oxyApplicationIdSchema,
  credentialId: oxyCredentialIdSchema,
  environment: inferenceEnvironmentSchema,
  lane: kaanaIdentityLaneSchema,
  applicationClass: kaanaIdentityApplicationClassSchema,
  applicationIsInternal: z.boolean(),
};

export const kaanaIdentityPrincipalSchema = z
  .discriminatedUnion('authority', [
    z.object({ ...principalFields, authority: z.literal('first_party') }).strict(),
    z
      .object({
        ...principalFields,
        authority: z.literal('external'),
        scopes: z.array(inferenceScopeSchema),
      })
      .strict(),
    z
      .object({
        authority: z.literal('session'),
        lane: kaanaIdentityLaneSchema,
        applicationId: oxyApplicationIdSchema,
        credentialId: oxyCredentialIdSchema,
        environment: inferenceEnvironmentSchema,
        applicationClass: kaanaIdentityApplicationClassSchema,
        applicationIsInternal: z.boolean(),
        /** Application ownership is not a payer decision or a session privilege. */
        applicationOwnerAccountId: oxyAccountIdSchema,
        verifiedRequester: z
          .object({
            subjectAccountId: oxyAccountIdSchema,
            principalUserId: delegatedUserIdSchema,
          })
          .strict(),
        /** A public client id attributes an app; it does not authenticate that app. */
        applicationBinding: z.enum(['session', 'platform', 'public_client_id']),
      })
      .strict(),
  ])
  .superRefine((principal, ctx) => {
    if ((principal.authority === 'session') !== (principal.lane === 'product_session')) {
      ctx.addIssue({
        code: 'custom',
        path: ['lane'],
        message: 'Session authority requires the product session lane exclusively',
      });
    }
    if (principal.authority === 'first_party' && principal.lane !== 'service_token') {
      ctx.addIssue({
        code: 'custom',
        path: ['lane'],
        message: 'First-party authority requires a workload service token',
      });
    }
  });

export const kaanaIdentityAssertionSchema = z
  .object({
    schemaVersion: z.literal(1),
    kind: z.literal('identity'),
    requestId: requestIdSchema,
    audience: z.literal('kaana.ai'),
    requestSha256: z.string().regex(/^[0-9a-f]{64}$/),
    issuedAt: inferenceTimestampSchema,
    expiresAt: inferenceTimestampSchema,
    principal: kaanaIdentityPrincipalSchema,
    /** Attribution only for workloads/keys; verified subjectAccountId for sessions. */
    userId: delegatedUserIdSchema.optional(),
  })
  .strict()
  .superRefine((assertion, ctx) => {
    if (
      assertion.principal.authority === 'session' &&
      String(assertion.userId) !== String(assertion.principal.verifiedRequester.subjectAccountId)
    ) {
      ctx.addIssue({
        code: 'custom',
        path: ['userId'],
        message: 'Session userId must be the verified subject account',
      });
    }
    const duration = Date.parse(assertion.expiresAt) - Date.parse(assertion.issuedAt);
    if (!(duration > 0 && duration <= IDENTITY_VALIDITY_WINDOW_MS)) {
      ctx.addIssue({
        code: 'custom',
        path: ['expiresAt'],
        message: 'Identity assertions are valid for at most 60 seconds',
      });
    }
  });

export type KaanaIdentityRequest = z.infer<typeof kaanaIdentityRequestSchema>;
export type KaanaIdentityPrincipal = z.infer<typeof kaanaIdentityPrincipalSchema>;
export type KaanaIdentityAssertion = z.infer<typeof kaanaIdentityAssertionSchema>;
