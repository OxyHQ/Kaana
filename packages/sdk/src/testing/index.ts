/**
 * `@kaana.ai/sdk/testing` — the real `Kaana` client against `https://kaana.ai/v1`
 * run in memory, for testing code that uses Kaana without reaching it.
 *
 * Nothing here is a second opinion about what Kaana accepts. The fake server
 * decides with the schemas and `check*` functions of `@kaana.ai/contracts`
 * that Kaana itself applies, against a catalogue that is what Kaana serves for
 * its voice models (`voiceCatalogue`, held to Kaana's own render by a Go test),
 * and the client in front of it is the one a product ships. So a request the
 * fake accepts is one Kaana accepts, and a refusal reaches the code under test
 * as the SDK's own error class with production's `code` and `param`.
 *
 * Served: `GET /v1/models`, `GET /v1/models/{id}`,
 * `POST /v1/audio/transcriptions` and `GET /v1/realtime`. A realtime session
 * is driven by the test (`server.sessions[n].emit(...)`, `.drop()`, `.end()`)
 * and records what it applied (`.commands`).
 */

import { modelSchema, type Model } from '@kaana.ai/contracts';
import { Kaana } from '../client.js';
import { voiceCatalogueSnapshot } from './catalogue.generated.js';
import { FakeKaanaServer, type FakeKaanaServerOptions } from './server.js';

export {
  FakeKaanaServer,
  type FakeKaanaServerOptions,
  type FakeRequest,
  FakeRealtimeSession,
  type FakeServerEventInput,
  type FakeTranscriptionResult,
  type FakeTranscriptionUpload,
} from './server.js';

/** Kaana's voice models as Kaana serves them: realtime, transcription and speech. */
export const voiceCatalogue: readonly Model[] = voiceCatalogueSnapshot.map((model) =>
  modelSchema.parse(model),
);

export interface FakeKaanaOptions extends Partial<FakeKaanaServerOptions> {
  /** The credential the client presents; the fake accepts any. */
  readonly credential?: string;
}

/** A `Kaana` client wired to an in-memory server, and the server. */
export function createFakeKaana(options: FakeKaanaOptions = {}): {
  readonly kaana: Kaana;
  readonly server: FakeKaanaServer;
} {
  const server = new FakeKaanaServer({
    models: options.models ?? voiceCatalogue,
    ...(options.transcribe === undefined ? {} : { transcribe: options.transcribe }),
  });
  const kaana = new Kaana({
    credential: options.credential ?? 'oxy_sk_fake',
    fetch: server.fetch,
    WebSocket: server.WebSocket,
    maxRetries: 0,
  });
  return { kaana, server };
}
