import { describe, expect, it } from 'bun:test';
import {
  modelCatalogueEntrySchema,
  modelPowerClassSchema,
  powerLevelSchema,
  routingProfileSchema,
} from './contract.js';

describe('power levels (contract set 3.4.0)', () => {
  it('names seven levels and five model classes, and neither contains a slash', () => {
    expect(powerLevelSchema.options).toEqual([
      'auto',
      'instant',
      'medium',
      'high',
      'xhigh',
      'pro',
      'ultra',
    ]);
    expect(modelPowerClassSchema.options).toEqual(['instant', 'medium', 'high', 'pro', 'ultra']);
    expect(modelPowerClassSchema.safeParse('xhigh').success).toBe(false);
    expect(modelPowerClassSchema.safeParse('auto').success).toBe(false);
  });

  it('carries the level and its effort on a routing profile', () => {
    const profile = routingProfileSchema.parse({
      schemaVersion: 1,
      routingProfileId: 'power-medium',
      slug: 'medium',
      displayName: 'Medium',
      optimiseFor: 'price',
      candidates: [{ modelReference: 'pub/model', priority: 0 }],
      isProductPreset: true,
      powerLevel: 'medium',
      reasoningEffort: 'low',
    });
    expect(profile.powerLevel).toBe('medium');
    expect(profile.reasoningEffort).toBe('low');
    expect(routingProfileSchema.safeParse({ ...profile, powerLevel: 'turbo' }).success).toBe(false);
  });

  it('keeps powerClass optional on a catalogue entry and closed when present', () => {
    const shape = modelCatalogueEntrySchema.shape.powerClass;
    expect(shape.safeParse(undefined).success).toBe(true);
    expect(shape.safeParse('pro').success).toBe(true);
    expect(shape.safeParse('xhigh').success).toBe(false);
  });
});
