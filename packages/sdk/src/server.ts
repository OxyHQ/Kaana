import { KAANA_API_BASE_URL } from '@kaana.ai/contracts';
import { Kaana } from './client.js';
import type { KaanaAuthenticatedTransport, KaanaOptions } from './http.js';

/**
 * The account an Oxy service acts for, and the person when they operate it
 * rather than own it (oxy ADR 0066): Oxy checks the service's first-party
 * declaration and the accounts live. No session, task or consent is sent.
 */
export type KaanaActingFor = { accountId: string; requesterAccountId?: string };
export interface KaanaServiceTransport extends KaanaAuthenticatedTransport {
  dispose(): void;
}
/** Structural: the application supplies its OxyServer and infrastructure provider. */
export interface KaanaServiceIdentity {
  createServiceClient(
    options: { baseURL: string; audience: 'kaana.ai' } & (
      | { actingFor?: KaanaActingFor; attribution?: never }
      | { attribution: { accountId: string }; actingFor?: never }
    ),
  ): KaanaServiceTransport;
}
export type KaanaServiceOptions = Omit<KaanaOptions, 'credential' | 'fetch'> &
  (
    | { actingFor?: KaanaActingFor; attribution?: never }
    | { attribution: { accountId: string }; actingFor?: never }
  );

/** Own HTTP and realtime share Oxy's workload lifecycle; external credentials use `new Kaana`. */
export function kaanaForOxyService(
  oxy: KaanaServiceIdentity,
  options: KaanaServiceOptions = {},
): Kaana & { dispose(): void } {
  if ('credential' in options || 'fetch' in options || (options.actingFor && options.attribution))
    throw new TypeError('Kaana: own service identity and attribution must be unambiguous');
  const baseURL = options.baseURL ?? KAANA_API_BASE_URL;
  const origin = new URL(baseURL);
  if (
    origin.username ||
    origin.password ||
    origin.search ||
    origin.hash ||
    !['http:', 'https:'].includes(origin.protocol) ||
    !(
      origin.origin === 'https://kaana.ai' ||
      ['localhost', '127.0.0.1', '[::1]'].includes(origin.hostname)
    ) ||
    origin.pathname.replace(/\/$/, '') !== '/v1'
  ) {
    throw new TypeError('Kaana: own service destination must be https://kaana.ai/v1 or loopback');
  }
  const actingFor = options.actingFor ? { ...options.actingFor } : undefined;
  const attribution = options.attribution ? { ...options.attribution } : undefined;
  const accountId = actingFor?.accountId ?? attribution?.accountId;
  if (accountId !== undefined && (!accountId || accountId.length > 64 || /\s/.test(accountId)))
    throw new TypeError('Kaana: invalid inference attribution');
  const transport = oxy.createServiceClient(
    attribution
      ? { baseURL, audience: 'kaana.ai', attribution }
      : { baseURL, audience: 'kaana.ai', ...(actingFor ? { actingFor } : {}) },
  );
  const client = new Kaana({
    transport,
    baseURL,
    ...(accountId ? { accountId } : {}),
    ...(options.WebSocket ? { WebSocket: options.WebSocket } : {}),
    ...(options.timeoutMs === undefined ? {} : { timeoutMs: options.timeoutMs }),
    ...(options.maxRetries === undefined ? {} : { maxRetries: options.maxRetries }),
  });
  return Object.assign(client, { dispose: () => transport.dispose() });
}
