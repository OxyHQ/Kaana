/** Valid public and internal values, shared by the round-trip, error and grant suites. */

export const requestId = 'req_01JQZ7Q0000000000000000000';
export const model = 'openai/gpt-5@2026-06-01';

export const publicError = {
  schemaVersion: 1,
  code: 'rate_limited',
  message: 'Too many requests for this credential. Retry shortly.',
  retryable: true,
  requestId,
  retryAfterMs: 1500,
  upstreamCategory: 'rate_limit',
} as const;

export const usage = [
  { unit: 'input_tokens', quantity: 314 },
  { unit: 'output_tokens', quantity: 204 },
] as const;

export const audio = {
  voices: [
    { id: 'rachel', name: 'Rachel', languages: ['en', 'es-ES'], gender: 'female' },
    { id: 'adam', name: 'Adam', languages: ['en'] },
  ],
  languages: ['en', 'es-ES'],
  audioFormats: { input: [], output: ['audio/mpeg', 'audio/pcm'] },
  speed: { min: 0.7, max: 1.2 },
  maxInputCharacters: 5000,
} as const;

const capabilities = {
  inputModalities: ['text'],
  outputModalities: ['audio'],
  tools: false,
  parallelToolCalls: false,
  structuredOutput: false,
  jsonMode: false,
  reasoning: false,
  reasoningEfforts: [],
  streaming: true,
  promptCaching: false,
  maxContextTokens: 5000,
  maxOutputTokens: 5000,
  apiFormats: ['audio_speech'],
};

const dataPolicy = {
  retainsPayloads: false,
  retentionDays: 0,
  trainsOnCustomerData: false,
  zeroDataRetentionAvailable: true,
  subprocessors: [],
};

export const speechModel = {
  schemaVersion: 1,
  modelId: 'elevenlabs/eleven-multilingual',
  publisher: { slug: 'elevenlabs', displayName: 'ElevenLabs' },
  displayName: 'Eleven Multilingual',
  currentRevision: 'v2',
  availableRevisions: ['v2'],
  capabilities,
  audio,
  license: {
    licenseId: 'elevenlabs-terms',
    displayName: 'ElevenLabs Terms',
    commercialUseAllowed: true,
    requiresAttribution: false,
  },
  provenance: { releaseKind: 'third_party_hosted' },
  regions: ['us-east-1'],
  dataPolicy,
  pricing: {
    priceVersionId: 'pv_eleven_1',
    currency: 'USD',
    unitPrices: [{ unit: 'characters', amount: '0.30', per: 1000, currency: 'USD' }],
  },
  deprecation: { status: 'active' },
  evaluations: [],
  powerClass: 'medium',
} as const;

export const generationCharged = {
  schemaVersion: 1,
  kind: 'charge',
  requestId,
  generationId: 'gen_1',
  applicationId: 'app_1',
  credentialId: 'cred_1',
  environment: 'production',
  outcome: 'completed',
  usageSource: 'provider_reported',
  units: [...usage],
  model,
  labels: { team: 'growth' },
  settledAt: '2026-10-15T09:41:03.000Z',
  receiptId: 'rcpt_1',
  priceSnapshot: {
    priceVersionId: 'pv_1',
    currency: 'USD',
    unitPrices: [
      { unit: 'input_tokens', amount: '1.25', per: 1000000, currency: 'USD' },
      { unit: 'output_tokens', amount: '10.00', per: 1000000, currency: 'USD' },
    ],
  },
  billedAmount: '0.002432500000',
  currency: 'USD',
  platformFeeOnly: false,
} as const;

export const generationMetered = {
  schemaVersion: 1,
  kind: 'metered_usage',
  requestId,
  applicationId: 'app_alia',
  credentialId: 'cred_alia',
  delegatedUserId: 'user_1',
  environment: 'production',
  outcome: 'completed',
  usageSource: 'provider_reported',
  units: [...usage],
  model,
  settledAt: '2026-10-15T09:41:03.000Z',
  meteredUsageId: '0b3f4a3e-5a6f-4c1d-9b7a-2f1e3d4c5b6a',
  tariff: { status: 'quoted', amount: '0.002432500000', currency: 'USD', priceVersionId: 'pv_1' },
  customerCharge: { status: 'not_charged' },
} as const;

const attribution = {
  principal: {
    billing: { accountId: 'acc_1' },
    applicationId: 'app_1',
    credentialId: 'cred_1',
    environment: 'production',
    inferenceScopes: ['inference:invoke'],
  },
  requestId,
} as const;

export const usageReport = {
  schemaVersion: 2,
  requestId,
  attribution,
  outcome: 'completed',
  units: [...usage],
  usageSource: 'provider_reported',
  resolvedModelReference: model,
  servingProvider: 'openai',
  deploymentId: 'dep_openai_gpt5_use1',
  routeSwitches: 0,
  fundingTier: 'credit',
  startedAt: '2026-10-15T09:41:00.000Z',
  completedAt: '2026-10-15T09:41:02.000Z',
} as const;

const limits = (lines: { modelLine: string; priceVersionId: string }[]) => ({
  admittedModelLines: lines,
  providerAllowlist: [],
  providerDenylist: [],
  allowedRegions: [],
  deniedRegions: [],
  requireZeroDataRetention: false,
  prohibitTrainingOnCustomerData: true,
  allowedLicenseIds: [],
  requireCommercialUseRights: true,
  sameModelFailover: true,
});

export const commercialGrant = {
  schemaVersion: 1,
  grantId: 'grant_1',
  requestId,
  operation: 'responses',
  issuedAt: '2026-10-15T09:41:00.000Z',
  executeBy: '2026-10-15T09:42:00.000Z',
  bodySha256: 'a'.repeat(64),
  principal: {
    billingAccountId: 'acc_1',
    applicationId: 'app_1',
    credentialId: 'cred_1',
    environment: 'production',
    scopes: ['inference:invoke'],
    lane: 'machine_credential',
  },
  economics: { treatment: 'commercial', policyVersion: 'econ_2026_10' },
  reservation: {
    reservationId: 'res_1',
    ceiling: { amount: '0.050000000000', currency: 'USD' },
  },
  steps: [
    {
      target: { kind: 'model', modelLine: 'openai/gpt-5' },
      limits: {
        ...limits([{ modelLine: 'openai/gpt-5', priceVersionId: 'pv_1' }]),
        maxPricePerRequest: { amount: '0.050000000000', currency: 'USD' },
      },
    },
  ],
  idempotencyKey: 'idem_1',
  client: { labels: { team: 'growth' } },
  routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
} as const;

/**
 * Alia Pro/Ultra under the internal relationship (ADR 0002, decision 4): the
 * level under a credit-only ceiling, then `xhigh` on the pilot tuples.
 */
export const aliaProGrant = {
  ...commercialGrant,
  grantId: 'grant_alia_pro',
  principal: { ...commercialGrant.principal, lane: 'service_token' },
  userId: 'user_1',
  economics: {
    treatment: 'internal_metered',
    policyVersion: 'econ_2026_10',
    relationshipId: 'alia-kaana',
  },
  reservation: undefined,
  capacity: { meteredUsageId: '0b3f4a3e-5a6f-4c1d-9b7a-2f1e3d4c5b6a' },
  steps: [
    {
      target: { kind: 'level', level: 'pro' },
      limits: {
        ...limits([
          { modelLine: 'anthropic/claude-opus-5', priceVersionId: 'pv_opus' },
          { modelLine: 'openai/gpt-5-pro', priceVersionId: 'pv_gpt5pro' },
        ]),
        maxFundingTier: 'credit',
        maxPricePerRequest: { amount: '1.00', currency: 'USD' },
      },
    },
    {
      target: { kind: 'level', level: 'xhigh' },
      limits: {
        ...limits([{ modelLine: 'openai/gpt-5', priceVersionId: 'pv_1' }]),
        maxPricePerRequest: { amount: '0.05', currency: 'USD' },
      },
    },
  ],
} as const;

export const settlement = {
  schemaVersion: 1,
  settlementId: 'stl_1',
  grantId: 'grant_1',
  requestId,
  reservationId: 'res_1',
  servedStep: 0,
  report: usageReport,
  createdAt: '2026-10-15T09:41:02.500Z',
} as const;
