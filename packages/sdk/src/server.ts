import { KAANA_API_BASE_URL } from '@kaana.ai/contracts';
import { Kaana } from './client.js';
import type { KaanaAuthenticatedTransport, KaanaOptions } from './http.js';

export type KaanaRequester = { accountId: string } & (
  | { token: string; taskId?: never; requesterContext?: never }
  | { taskId: string; token?: never; requesterContext?: never }
  | { requesterContext: string; token?: never; taskId?: never }
);
export interface KaanaServiceTransport extends KaanaAuthenticatedTransport {
  dispose(): void;
}
/** Structural: the application supplies its OxyServer and infrastructure provider. */
export interface KaanaServiceIdentity {
  createServiceClient(
    options: { baseURL: string; audience: 'kaana.ai' } & (
      | { requester?: KaanaRequester; attribution?: never }
      | { attribution: { accountId: string }; requester?: never }
    ),
  ): KaanaServiceTransport;
}
export type KaanaServiceOptions = Omit<KaanaOptions, 'credential' | 'fetch'> &
  (
    | { requester?: KaanaRequester; attribution?: never }
    | { attribution: { accountId: string }; requester?: never }
  );

/** Own HTTP and realtime share Oxy's workload lifecycle; external credentials use `new Kaana`. */
export function kaanaForOxyService(
  oxy: KaanaServiceIdentity,
  options: KaanaServiceOptions = {},
): Kaana & { dispose(): void } {
  if ('credential' in options || 'fetch' in options || (options.requester && options.attribution))
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
  const requester = options.requester ? { ...options.requester } : undefined;
  const attribution = options.attribution ? { ...options.attribution } : undefined;
  const accountId = requester?.accountId ?? attribution?.accountId;
  if (accountId !== undefined && (!accountId || accountId.length > 64 || /\s/.test(accountId)))
    throw new TypeError('Kaana: invalid inference attribution');
  const transport = oxy.createServiceClient(
    attribution
      ? { baseURL, audience: 'kaana.ai', attribution }
      : { baseURL, audience: 'kaana.ai', ...(requester ? { requester } : {}) },
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
