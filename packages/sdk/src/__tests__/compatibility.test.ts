/**
 * What Kaana sends, read by the releases that run in production.
 *
 * `compat/kaana-sends.generated.json` is recorded by Kaana's own Go test
 * (`TestTheCompatibilityFixturesAreWhatKaanaSends`) through the public API: a
 * catalogue, a transcription, every public frame of a realtime session, and
 * the authorizations and settlements Oxy receives. `voiceCatalogueSnapshot` is
 * what Kaana serves for its voice models. Here they are parsed by the exact
 * published releases a deployed reader uses — `@kaana.ai/sdk` 1.0.1 with its
 * `@kaana.ai/contracts` 4.4.0 (Alia), `@kaana.ai/contracts` 4.4.1 (Oxy) and
 * 5.0.0 (`@kaana.ai/sdk` 2.0.0) — so a change that would break one fails
 * before it merges. A release leaves this list only when nothing deployed
 * runs it any more.
 */

import { describe, expect, it } from 'bun:test';
import * as contracts440 from 'kaana-contracts-4.4.0';
import * as internal441 from 'kaana-contracts-4.4.1/internal';
import * as contracts500 from 'kaana-contracts-5.0.0';
import { Kaana as Kaana101 } from 'kaana-sdk-1.0.1';
import { voiceCatalogueSnapshot } from '../testing/catalogue.generated.js';
import recorded from './compat/kaana-sends.generated.json' with { type: 'json' };

const catalogues = [
  { name: 'the voice models', body: { data: voiceCatalogueSnapshot } },
  { name: 'a mixed catalogue', body: recorded.catalogue },
];

/** A client of a given release whose every request is answered with `body`. */
function answering(body: unknown): Kaana101 {
  return new Kaana101({
    credential: 'oxy_sk_compat',
    maxRetries: 0,
    fetch: async () =>
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json', 'Kaana-Request-Id': 'req_compat' },
      }),
  });
}

describe('what Kaana sends parses in the releases running in production', () => {
  for (const { name, body } of catalogues) {
    it(`@kaana.ai/sdk 1.0.1 lists ${name}`, async () => {
      const models = await answering(body).models.list();
      expect(models.length).toBe(body.data.length);
    });

    it(`@kaana.ai/contracts 4.4.0 and 5.0.0 parse ${name}`, () => {
      for (const schema of [contracts440.modelListSchema, contracts500.modelListSchema]) {
        const parsed = schema.safeParse(body);
        expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
      }
    });
  }

  it('@kaana.ai/sdk 1.0.1 reads a transcription', async () => {
    const transcription = await answering(recorded.transcription).audio.transcriptions.create({
      model: 'stub/whisper',
      file: new Uint8Array([82, 73, 70, 70]),
      mediaType: 'audio/wav',
    });
    expect(transcription.text).toBe(recorded.transcription.text);
  });

  it('every realtime frame parses in @kaana.ai/contracts 4.4.0 and 5.0.0', () => {
    expect(recorded.realtimeEvents.length).toBeGreaterThan(10);
    for (const frame of recorded.realtimeEvents) {
      for (const schema of [
        contracts440.realtimePublicServerEventSchema,
        contracts500.realtimePublicServerEventSchema,
      ]) {
        const parsed = schema.safeParse(frame);
        expect(parsed.success ? [] : [frame.type, parsed.error.issues]).toEqual([]);
      }
    }
  });

  it('Oxy on @kaana.ai/contracts 4.4.1 accepts every authorization and settlement', () => {
    for (const body of recorded.authorizations) {
      const parsed = internal441.authorizationRequestSchema.safeParse(body);
      expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
    }
    for (const body of recorded.settlements) {
      const parsed = internal441.settlementSchema.safeParse(body);
      expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
    }
  });
});
