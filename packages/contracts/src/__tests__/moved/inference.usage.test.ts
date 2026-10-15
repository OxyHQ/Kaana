import { describe, expect, it } from 'bun:test';
import {
  exactDecimalSchema,
  INFERENCE_MONEY_SCALE,
  normalizedUsageReportSchema,
  safeParseContract,
  unitPriceSchema,
  usageQuantitySchema,
  USAGE_UNITS,
} from './contract.js';

const attribution = {
  principal: {
    billing: { accountId: 'acc_1' },
    applicationId: 'app_1',
    credentialId: 'cred_1',
    environment: 'production' as const,
    inferenceScopes: ['inference:invoke'],
  },
  requestId: 'req_1',
};

const report = {
  schemaVersion: 2 as const,
  requestId: 'req_1',
  attribution,
  outcome: 'completed' as const,
  units: [{ unit: 'output_tokens', quantity: 204 }],
  usageSource: 'provider_reported' as const,
  resolvedModelReference: 'openai/gpt-5@2026-06-01',
  servingProvider: 'openai',
  deploymentId: 'dep_openai_gpt5_usw2',
  routeSwitches: 0,
  startedAt: '2026-08-15T09:41:00.000Z',
  completedAt: '2026-08-15T09:41:02.000Z',
};

const priceSnapshot = {
  priceVersionId: 'pv_1',
  currency: 'USD',
  unitPrices: [{ unit: 'output_tokens', amount: '15.00', per: 1000000, currency: 'USD' }],
};

describe('money is exact', () => {
  it('declares one scale for every amount and price', () => {
    // ADR 0009: money is exact NUMERIC with sub-minor-unit precision, NOT
    // integer minor units — rounding per request would make a bill depend on
    // how a client chunked its work.
    expect(INFERENCE_MONEY_SCALE).toBe(12);
    expect(exactDecimalSchema.safeParse('0.000003000000').success).toBe(true);
    expect(exactDecimalSchema.safeParse('0.0000030000001').success).toBe(false);
  });

  it('refuses a JavaScript number as an amount at all', () => {
    expect(exactDecimalSchema.safeParse(1806).success).toBe(false);
    expect(exactDecimalSchema.safeParse(18.06).success).toBe(false);
  });

  it('carries amounts and prices as exact decimal strings with one spelling', () => {
    expect(exactDecimalSchema.safeParse('0.0000015').success).toBe(true);
    expect(exactDecimalSchema.safeParse('15').success).toBe(true);
    expect(exactDecimalSchema.safeParse('015').success).toBe(false);
    expect(exactDecimalSchema.safeParse('-1.5').success).toBe(false);
    expect(exactDecimalSchema.safeParse('1.5e-3').success).toBe(false);
    expect(exactDecimalSchema.safeParse(1.5).success).toBe(false);
  });

  it('keeps unit counts free of money and money free of units', () => {
    expect(Object.keys(usageQuantitySchema.shape)).toEqual(['unit', 'quantity']);
    expect(
      usageQuantitySchema.safeParse({ unit: 'output_tokens', quantity: 1, currency: 'USD' })
        .success,
    ).toBe(false);
    expect(usageQuantitySchema.safeParse({ unit: 'output_tokens', quantity: 1.5 }).success).toBe(
      false,
    );
  });

  it('meters time in integer milliseconds, so no quantity is ever fractional', () => {
    expect(USAGE_UNITS).toContain('audio_input_milliseconds');
    expect(USAGE_UNITS).not.toContain('audio_seconds');
    expect(
      usageQuantitySchema.safeParse({ unit: 'audio_input_milliseconds', quantity: 12500 }).success,
    ).toBe(true);
  });

  it('quotes a unit price per a stated quantity of units', () => {
    expect(safeParseContract(unitPriceSchema, priceSnapshot.unitPrices[0])).toEqual(
      priceSnapshot.unitPrices[0],
    );
    expect(unitPriceSchema.safeParse({ ...priceSnapshot.unitPrices[0], per: 0 }).success).toBe(
      false,
    );
  });
});

describe('normalizedUsageReportSchema', () => {
  it('requires the exact deployment identity even with no measured units', () => {
    const { deploymentId: _omitted, ...withoutDeployment } = report;
    expect(normalizedUsageReportSchema.safeParse(withoutDeployment).success).toBe(false);
  });

  it('reports units and route, never money', () => {
    const keys = Object.keys(normalizedUsageReportSchema.shape);
    expect(keys).toContain('units');
    expect(keys).toContain('resolvedModelReference');
    for (const money of ['billedAmount', 'amount', 'currency', 'upstreamCost', 'priceSnapshot']) {
      expect(keys).not.toContain(money);
    }
  });

  it('marks an estimate as an estimate', () => {
    const estimated = normalizedUsageReportSchema.parse({
      ...report,
      usageSource: 'estimated',
    });
    expect(estimated.usageSource).toBe('estimated');
    expect(
      normalizedUsageReportSchema.safeParse({ ...report, usageSource: 'probably' }).success,
    ).toBe(false);
  });

  it('rejects a request that completed before it started', () => {
    expect(
      normalizedUsageReportSchema.safeParse({
        ...report,
        completedAt: '2026-08-15T09:40:59.000Z',
      }).success,
    ).toBe(false);
  });

  it('rejects a unit reported twice instead of totalled', () => {
    expect(
      normalizedUsageReportSchema.safeParse({
        ...report,
        units: [
          { unit: 'output_tokens', quantity: 100 },
          { unit: 'output_tokens', quantity: 104 },
        ],
      }).success,
    ).toBe(false);
  });

  it('REFUSES a completed report with no units, so a free request is unrepresentable', () => {
    // The load-bearing case. Settlement prices every reported unit and sums, so
    // an empty list on a `completed` report is a request served for nothing —
    // which is exactly what a provider that omits its usage block produces. The
    // policy is refuse-and-release, so the shape must not parse.
    expect(normalizedUsageReportSchema.safeParse({ ...report, units: [] }).success).toBe(false);

    const refusal = normalizedUsageReportSchema.safeParse({ ...report, units: [] });
    expect(refusal.success).toBe(false);
    if (!refusal.success) {
      expect(refusal.error.issues.some((issue) => issue.path.join('.') === 'units')).toBe(true);
    }
  });

  it('accepts a failed report with no units, because that is how failure is reported', () => {
    // `outcomeFor` in Kaana's `internal/kaana/executor.go`
    // DERIVES `failed` from having no units and `partial` from having some, so an
    // unconditional minimum would refuse every failure — and a refused report is
    // a request that ran, cost money upstream, and can never be settled.
    const failed = normalizedUsageReportSchema.parse({
      ...report,
      outcome: 'failed',
      units: [],
      usageSource: 'estimated',
    });
    expect(failed.units).toEqual([]);
  });

  it('accepts a cancelled report with no units, for a client that stopped before output', () => {
    const cancelled = normalizedUsageReportSchema.parse({
      ...report,
      outcome: 'cancelled',
      units: [],
      usageSource: 'estimated',
    });
    expect(cancelled.units).toEqual([]);
  });

  it('still accepts a completed report that has units', () => {
    // Positive control: the refinement rejects the empty list and nothing else.
    const completed = normalizedUsageReportSchema.parse(report);
    expect(completed.outcome).toBe('completed');
    expect(completed.units).toHaveLength(1);
  });
});
