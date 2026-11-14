/**
 * What Kaana sends, read by the releases that run in production.
 *
 * `compat/kaana-sends.generated.json` is recorded by Kaana's own Go test
 * (`TestTheCompatibilityFixturesAreWhatKaanaSends`) through the public API: a
 * catalogue, a transcription, every public frame of a realtime session, and
 * the authorizations and settlements Oxy receives. Here they are parsed by the
 * exact published releases a deployed reader uses, so a change that would
 * break one fails before it merges. A release leaves this list only when
 * nothing deployed runs it any more.
 *
 * `GET /v1/models` is rendered in the set a request states in
 * `Kaana-Contract-Version`, without the `CATALOGUE_ADDITIONS` that set lacks.
 * `compat/catalogue-by-set.generated.json`
 * (`TestTheCatalogueIsRenderedInTheReadersContractSet`, a catalogue holding
 * every addition) and `compat/voice-catalogue-by-set.generated.json`
 * (`TestTheVoiceCatalogueByReaderSetIsWhatKaanaServes`, Kaana's voice models)
 * are both recorded as each pinned release receives them: each release states
 * its set and lists what it is sent. `CATALOGUE_ADDITIONS` is held to the
 * difference between the workspace's `modelSchema` and each release's, so a
 * value the catalogue gains that a deployed reader refuses fails here until it
 * is listed, and Kaana leaves out what is listed.
 */

import { describe, expect, it } from 'bun:test';
import { CATALOGUE_ADDITIONS, modelListSchema, modelSchema } from '@kaana.ai/contracts';
import * as contracts440 from 'kaana-contracts-4.4.0';
import * as internal441 from 'kaana-contracts-4.4.1/internal';
import * as contracts500 from 'kaana-contracts-5.0.0';
import * as contracts801 from 'kaana-contracts-8.0.1';
import * as contracts820 from 'kaana-contracts-8.2.0';
import * as contracts8150 from 'kaana-contracts-8.15.0';
import * as contracts8160 from 'kaana-contracts-8.16.0';
import { Kaana as Kaana101 } from 'kaana-sdk-1.0.1';
import { Kaana as Kaana330 } from 'kaana-sdk-3.3.0';
import { Kaana as Kaana350 } from 'kaana-sdk-3.5.0';
import { Kaana as Kaana3160 } from 'kaana-sdk-3.16.0';
import bySet from './compat/catalogue-by-set.generated.json' with { type: 'json' };
import recorded from './compat/kaana-sends.generated.json' with { type: 'json' };
import voiceBySet from './compat/voice-catalogue-by-set.generated.json' with { type: 'json' };

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
  it('@kaana.ai/sdk 1.0.1, contracts 4.4.0 and 5.0.0 read a mixed catalogue', async () => {
    const models = await answering(recorded.catalogue).models.list();
    expect(models.length).toBe(recorded.catalogue.data.length);
    for (const schema of [contracts440.modelListSchema, contracts500.modelListSchema]) {
      const parsed = schema.safeParse(recorded.catalogue);
      expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
    }
  });

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

  // Reasoning continuity (contract set 8.6.0): the delta gained an optional
  // `opaque`, sent only to a request that carried `reasoning`. A reader of an
  // older contract must still read the stream; a reader that never asked never
  // receives the token, nor a `reasoning` part in a whole answer, which it
  // would refuse (Kaana ADR 0009 decision 2).
  it('a reasoning delta carrying a continuation token parses in every deployed contract', () => {
    const delta = {
      schemaVersion: 1,
      type: 'delta',
      requestId: 'req_compat',
      sequence: 3,
      outputIndex: 0,
      channel: 'reasoning',
      text: '',
      opaque: 'eyJmIjoiYSJ9',
    };
    for (const schema of [
      contracts440.responseStreamEventSchema,
      contracts500.responseStreamEventSchema,
    ]) {
      const parsed = schema.safeParse(delta);
      expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
    }
  });

  it('a whole answer without reasoning is the answer deployed contracts always parsed', () => {
    const answer = {
      schemaVersion: 1,
      requestId: 'req_compat',
      model: 'stub/model@2026-05-01',
      finishReason: 'stop',
      output: [{ role: 'assistant', content: [{ type: 'text', text: 'ok' }] }],
      usage: [{ unit: 'output_tokens', quantity: 1 }],
      routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 1 },
      latencyMs: 5,
    };
    for (const schema of [contracts440.responseSchema, contracts500.responseSchema]) {
      const parsed = schema.safeParse(answer);
      expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
    }
  });
});

type Def = {
  type: string;
  innerType?: unknown;
  in?: unknown;
  element?: unknown;
  shape?: Record<string, unknown>;
  catchall?: unknown;
  entries?: Record<string, string>;
};
const defOf = (schema: unknown): Def => (schema as { _zod: { def: Def } })._zod.def;

function unwrap(schema: unknown): unknown {
  let current = schema;
  for (;;) {
    const def = defOf(current);
    if (def.type === 'optional' || def.type === 'default' || def.type === 'nullable') {
      current = def.innerType;
    } else if (def.type === 'pipe') {
      current = def.in;
    } else {
      return current;
    }
  }
}

/**
 * What `modelSchema` has that a release's refuses, in the shape of
 * `CATALOGUE_ADDITIONS`: a field of a strict object the release lacks, or an
 * enum member it lacks. A field of a non-strict object is stripped, not
 * refused, so it needs no entry.
 */
function refusedBy(current: unknown, release: unknown, path: string[] = []): string[] {
  const now = unwrap(current);
  const then = unwrap(release);
  const [nowDef, thenDef] = [defOf(now), defOf(then)];
  if (nowDef.type !== thenDef.type)
    return [`${path.join('.')} changed from ${thenDef.type} to ${nowDef.type}`];
  switch (nowDef.type) {
    case 'object': {
      const strict = thenDef.catchall !== undefined && defOf(thenDef.catchall).type === 'never';
      return Object.entries(nowDef.shape ?? {}).flatMap(([name, field]) => {
        const old = thenDef.shape?.[name];
        if (old === undefined) return strict ? [[...path, name].join('.')] : [];
        return refusedBy(field, old, [...path, name]);
      });
    }
    case 'array':
      return refusedBy(nowDef.element, thenDef.element, path);
    case 'enum': {
      const known = new Set(Object.values(thenDef.entries ?? {}));
      return Object.values(nowDef.entries ?? {})
        .filter((member) => !known.has(member))
        .map((member) => `${path.join('.')}=${member}`);
    }
    default:
      return [];
  }
}

const isBefore = (reader: string, since: string): boolean => {
  const [a, b] = [reader.split('.').map(Number), since.split('.').map(Number)];
  for (let index = 0; index < 3; index += 1) {
    if (a[index] !== b[index]) return (a[index] ?? 0) < (b[index] ?? 0);
  }
  return false;
};

type Listing = { data: unknown[] };
type Fetch = (url: string | URL | Request, init?: RequestInit) => Promise<Response>;
type Lister = (fetch: Fetch) => { models: { list(): Promise<unknown[]> } };

/** A release's `models.list`, answered with `body`, and the headers its request carried. */
async function listWith(
  client: Lister,
  body: Listing,
): Promise<{ listed: number; headers: Headers }> {
  let headers = new Headers();
  const kaana = client(async (_url, init) => {
    headers = new Headers(init?.headers);
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json', 'Kaana-Request-Id': 'req_compat' },
    });
  });
  return { listed: (await kaana.models.list()).length, headers };
}

const options = (fetch: Fetch) => ({
  credential: 'oxy_sk_compat',
  maxRetries: 0,
  fetch,
});

/**
 * Every release pinned here, with the set it states (none before
 * `@kaana.ai/sdk` 2.0.0) and the client that lists the catalogue, when the
 * release is an SDK. Each set is one of Kaana's `PinnedReaderSets`
 * (`internal/publicapi/pinned_readers_test.go`), which records the catalogue
 * for it. `@kaana.ai/sdk` 3.17.0 is the workspace's own version, so it is
 * pinned by its `@kaana.ai/contracts` 8.16.0 alone.
 */
const readers: readonly {
  name: string;
  states: string | null;
  contracts: {
    INFERENCE_CONTRACT_VERSION: string;
    modelListSchema: typeof modelListSchema;
    modelSchema: unknown;
  };
  client?: Lister;
}[] = [
  {
    name: '@kaana.ai/sdk 1.0.1 (contracts 4.4.0)',
    states: null,
    contracts: contracts440 as never,
    client: (fetch) => new Kaana101(options(fetch)),
  },
  {
    name: '@kaana.ai/contracts 5.0.0 (sdk 2.0.0)',
    states: '5.0.0',
    contracts: contracts500 as never,
  },
  {
    name: '@kaana.ai/sdk 3.3.0 (contracts 8.0.1, Clarity)',
    states: '8.0.0',
    contracts: contracts801 as never,
    client: (fetch) => new Kaana330(options(fetch)),
  },
  {
    name: '@kaana.ai/sdk 3.5.0 (contracts 8.2.0, Gemies and Mention)',
    states: '8.2.0',
    contracts: contracts820 as never,
    client: (fetch) => new Kaana350(options(fetch)),
  },
  {
    name: '@kaana.ai/sdk 3.16.0 (contracts 8.15.0, Syra)',
    states: '8.15.0',
    contracts: contracts8150 as never,
    client: (fetch) => new Kaana3160(options(fetch)),
  },
  {
    name: '@kaana.ai/contracts 8.16.0 (sdk 3.17.0)',
    states: '8.16.0',
    contracts: contracts8160 as never,
  },
];

const recordings: readonly [string, Record<string, Listing>][] = [
  ['a catalogue with every addition', bySet],
  ["Kaana's voice models", voiceBySet],
];

const hasLive = (body: Listing): boolean =>
  JSON.stringify(body).includes('"sessionKinds":["conversation","live"]') &&
  JSON.stringify(body).includes('"defaultKinds":["live"]');

describe('the catalogue is rendered in the set each deployed reader states', () => {
  for (const reader of readers) {
    for (const [what, recording] of recordings) {
      it(`${reader.name} lists ${what} as Kaana renders it for its set`, async () => {
        if (reader.states !== null)
          expect(reader.contracts.INFERENCE_CONTRACT_VERSION).toBe(reader.states);
        const body = recording[reader.states ?? 'unstated'];
        // A set Kaana does not record for is a reader nobody tested.
        expect(body).toBeDefined();
        if (body === undefined) return;
        const parsed = reader.contracts.modelListSchema.safeParse(body);
        expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
        if (reader.client !== undefined) {
          const { listed, headers } = await listWith(reader.client, body);
          expect(headers.get('Kaana-Contract-Version')).toBe(reader.states);
          expect(listed).toBe(body.data.length);
        }
      });
    }

    it(`CATALOGUE_ADDITIONS lists exactly what ${reader.name} refuses`, () => {
      const set = reader.contracts.INFERENCE_CONTRACT_VERSION;
      const listed = CATALOGUE_ADDITIONS.filter((entry) => isBefore(set, entry.since)).map(
        (entry) => `${entry.path.join('.')}${entry.member === undefined ? '' : `=${entry.member}`}`,
      );
      expect(refusedBy(modelSchema, reader.contracts.modelSchema).sort()).toEqual(listed.sort());
    });
  }

  it('a reader on 8.16.0 is sent live and its default line, which 8.2.0 refuses', () => {
    expect(hasLive(bySet['8.16.0'])).toBe(true);
    for (const older of ['unstated', '5.0.0', '8.0.0', '8.2.0', '8.15.0'] as const) {
      expect(hasLive(bySet[older])).toBe(false);
    }
    const parsed = modelListSchema.safeParse(bySet['8.16.0']);
    expect(parsed.success ? [] : parsed.error.issues).toEqual([]);
    // The control: the recording carries what an older reader refuses, so the
    // tests above would fail if Kaana sent it to one.
    expect(contracts820.modelListSchema.safeParse(bySet['8.16.0']).success).toBe(false);
  });
});
