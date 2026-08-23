/**
 * Kaana's health projection.
 *
 * ## Why these shapes are declared and not imported
 *
 * Unlike `envelope.ts` and `stream.ts`, this response is NOT a published
 * contract shape. `@oxyhq/contracts` covers what the control plane and the data
 * plane exchange about a REQUEST; the health surface is `internal/httpapi`'s own
 * operator projection, assembled there from `provider.Health`,
 * `inventory.SnapshotStatus` and `rotation.Health`. There is nothing to import
 * it from, so it is declared here — and it is worth being explicit that this
 * half is a mirror and CAN drift, while the request and stream halves cannot,
 * because they are the published schemas themselves.
 *
 * Every object below is deliberately non-strict. An operator surface grows
 * fields, and a health check that fails because the data plane started reporting
 * one more number is a health check reporting an outage that is not happening.
 *
 * ## What is not here
 *
 * No upstream URL, no region capacity, no credential and not even a truncated
 * hash of one — a fingerprint of a secret confirms a guessed secret. No upstream
 * cost either: `internal/providercost` is an operator number that never enters a
 * response of any kind, so there is nothing on this surface to model.
 *
 * The route is signature-gated like the inference route, over an EMPTY body —
 * see `KaanaClient.health`.
 */

import { z } from 'zod';

/** Whether an adapter's upstream answered a probe. */
export const kaanaProviderHealthStatusSchema = z.enum([
  'ok',
  'degraded',
  'unavailable',
  // Distinct from `unavailable` on purpose: an operator reading "unavailable"
  // goes to look at the provider, and the answer is that Kaana holds no
  // credential for it.
  'unconfigured',
]);

/**
 * One key's position in a provider's pool, and whether it is in service.
 *
 * A key's identity outside `internal/provider` is its position, never the secret
 * and never a hash of it.
 */
export const kaanaKeyHealthSchema = z.object({
  position: z.number().int().nonnegative(),
  /** `usable`, or the retirement that took it out. */
  state: z.string(),
  retiredUntil: z.string().optional(),
});

export const kaanaKeyPoolHealthSchema = z.object({
  declared: z.number().int().nonnegative(),
  usable: z.number().int().nonnegative(),
  keys: z.array(kaanaKeyHealthSchema),
});

export const kaanaProviderHealthSchema = z.object({
  provider: z.string(),
  status: kaanaProviderHealthStatusSchema,
  checkedAt: z.string(),
  latencyMs: z.number().int().nonnegative().optional(),
  detail: z.string().optional(),
  /**
   * The state of this provider's key pool. A pool draining towards empty is
   * otherwise invisible until the request that finds it empty.
   */
  credentials: kaanaKeyPoolHealthSchema.optional(),
});

/**
 * What the data plane is serving from.
 *
 * `servesUnpinnedReferences` goes false once the snapshot has aged past its
 * horizon: pinned references keep being served because the mapping from
 * immutable weights to a provider's model id cannot go stale, while which
 * revision is CURRENT is Oxy's decision and does. A caller routing by an
 * unpinned `publisher/model` reference reads this to tell "the model does not
 * exist" from "the catalogue stopped advancing".
 */
export const kaanaSnapshotStatusSchema = z.object({
  snapshotId: z.string(),
  issuedAt: z.string(),
  ageSeconds: z.number().int(),
  maxAgeSeconds: z.number().int(),
  servesUnpinnedReferences: z.boolean(),
  lastReloadError: z.string().optional(),
});

/**
 * A route's breaker state.
 *
 * `open` is "we stopped asking", not "the provider is down" — only this surface
 * says which.
 */
export const kaanaDeploymentHealthSchema = z.object({
  deploymentId: z.string(),
  state: z.enum(['closed', 'open', 'half_open']),
  score: z.number(),
  consecutiveFailures: z.number().int().nonnegative(),
  probesAt: z.string().optional(),
  provider: z.string(),
});

export const kaanaHealthSchema = z.object({
  /**
   * The version of the contract SET both sides were built against — the
   * handshake `INFERENCE_CONTRACT_VERSION` exists for. A mismatch is a skew no
   * per-message `schemaVersion` can express: a new enum member or a loosened
   * refinement produces a message the older side refuses with no version
   * difference to explain it.
   */
  contractVersion: z.string(),
  checkedAt: z.string(),
  providers: z.array(kaanaProviderHealthSchema),
  configuration: kaanaSnapshotStatusSchema,
  deployments: z.array(kaanaDeploymentHealthSchema),
});

export type KaanaProviderHealth = z.infer<typeof kaanaProviderHealthSchema>;
export type KaanaSnapshotStatus = z.infer<typeof kaanaSnapshotStatusSchema>;
export type KaanaDeploymentHealth = z.infer<typeof kaanaDeploymentHealthSchema>;
export type KaanaHealth = z.infer<typeof kaanaHealthSchema>;
