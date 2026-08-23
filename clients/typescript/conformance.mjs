/**
 * Pushes the wire fixtures this repository GENERATES through the built client.
 *
 * `tools/contract/validate.mjs` proves the shapes Relay produces parse under the
 * published Zod schemas. This proves the client can READ them — through the SSE
 * framing, the frame-name dispatch and the error path a caller actually meets,
 * rather than by handing the value straight to a schema, which would test the
 * schema and not the client.
 *
 * It is a separate script from `npm test` on purpose. The unit tests are
 * hermetic and run anywhere; this one needs `go test ./internal/contract/...`
 * to have written `internal/contract/testdata/wire/` first, exactly as
 * `validate.mjs` does, because that directory is generated and gitignored.
 *
 * It refuses to report success unless:
 *
 *  - it read at least one fixture from each directory — an empty run and a clean
 *    run otherwise look identical;
 *  - every valid fixture it is responsible for DECODED, and decoded back to the
 *    same value;
 *  - every invalid fixture it is responsible for was REFUSED, and refused as a
 *    protocol error rather than reported to a caller as a real answer. These are
 *    the vacuity floor: a client that accepted everything would pass the valid
 *    ones and fail here;
 *  - every fixture is either decoded or named in NOT_DECODED, whose schema set
 *    is asserted exactly — so a schema the contract adds later fails this rather
 *    than being silently skipped.
 *
 * Usage: `npm ci && npm run build && npm run conformance`, after
 * `go test ./internal/contract/...`.
 */
import { deepStrictEqual } from 'node:assert';
import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

import {
  decodeKaanaFrame,
  readSseFrames,
  KaanaClient,
  KaanaInferenceError,
  KaanaProtocolError,
  KAANA_FRAME_STREAM_EVENT,
  KAANA_FRAME_USAGE_REPORT,
  ed25519PrivateKeyFromSeed,
} from './dist/src/index.js';

const FIXTURE_ROOT = resolve(
  import.meta.dirname,
  '..',
  '..',
  'internal',
  'contract',
  'testdata',
  'wire',
);

/**
 * Schemas this client does not decode, and why.
 *
 * The set is asserted exactly rather than as a floor, because "the client had
 * nothing to do with this fixture" is also what an unrouted new schema looks
 * like.
 */
const NOT_DECODED = {
  inferenceRequestSchema:
    'the client BUILDS a request envelope and never reads one; the shape it produces is what `tools/contract/validate.mjs` parses',
};

function readFixtures(kind) {
  const dir = join(FIXTURE_ROOT, kind);
  let entries;
  try {
    entries = readdirSync(dir)
      .filter((name) => name.endsWith('.json'))
      .sort();
  } catch (error) {
    throw new Error(
      `cannot read ${dir}: run \`go test ./internal/contract/...\` first (${error.message})`,
    );
  }
  return entries.map((name) => ({
    file: join(kind, name),
    ...JSON.parse(readFileSync(join(dir, name), 'utf8')),
  }));
}

/** Frames one payload exactly as `internal/sse`'s writer does. */
async function* onSseWire(frameName, payload) {
  yield new TextEncoder().encode(`event: ${frameName}\ndata: ${JSON.stringify(payload)}\n\n`);
}

/** Reads a payload back through the SSE reader and the frame dispatch. */
async function decodeFramed(frameName, payload) {
  const decoded = [];
  for await (const frame of readSseFrames(onSseWire(frameName, payload))) {
    decoded.push(decodeKaanaFrame(frame));
  }
  if (decoded.length !== 1) {
    throw new Error(`one frame in, ${decoded.length} out`);
  }
  return decoded[0];
}

/**
 * Drives the real client against a 502 carrying the fixture as its body, which
 * is the only path on which a caller meets an error body.
 *
 * A signing key is generated here and thrown away; no key material is committed,
 * and nothing in this script reaches a network.
 */
async function decodeErrorBody(payload) {
  const { generateKeyPairSync } = await import('node:crypto');
  const { privateKey } = generateKeyPairSync('ed25519');
  const seed = privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32);
  const client = new KaanaClient({
    baseUrl: 'https://kaana.invalid',
    signingKey: { keyId: 'conformance', privateKey: ed25519PrivateKeyFromSeed(seed) },
    fetch: () =>
      Promise.resolve(
        new Response(JSON.stringify(payload), {
          status: 502,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
  });
  // The envelope never leaves the injected fetch, so its content is irrelevant
  // beyond being one the builder accepts.
  const envelope = {
    schemaVersion: 1,
    attribution: {
      principal: {
        billing: { accountId: 'acc_conformance' },
        applicationId: 'app_conformance',
        credentialId: 'cred_conformance',
        environment: 'development',
        inferenceScopes: ['inference:invoke'],
      },
      requestId: 'req_conformance',
    },
    target: { kind: 'model', modelReference: 'openai/gpt-4o-mini' },
    modality: 'text',
    input: { format: 'messages', messages: [{ role: 'user', content: [{ type: 'text', text: 'x' }] }] },
    stream: true,
    sampling: {},
    tools: [],
    client: {
      apiFormat: 'chat_completions',
      endpoint: '/v1/chat/completions',
      receivedAt: '2026-08-24T00:00:00.000Z',
    },
    routingPolicy: { routingPolicyId: 'rp_conformance', policyVersion: 1 },
  };
  for await (const _frame of client.stream(envelope)) {
    throw new Error('the client yielded a frame from a 502 response');
  }
  throw new Error('the client returned without throwing on a 502 response');
}

/**
 * Decodes one fixture the way a caller would meet it, or reports that this
 * client is not the thing that reads it.
 *
 * Returns `{ handled: false }` for a schema in NOT_DECODED and
 * `{ handled: true, value }` otherwise, throwing whatever the client throws.
 */
async function decode(fixture) {
  switch (fixture.schema) {
    case 'inferenceStreamEventSchema': {
      const frame = await decodeFramed(KAANA_FRAME_STREAM_EVENT, fixture.value);
      return { handled: true, value: frame.event };
    }
    case 'normalizedUsageReportSchema': {
      const frame = await decodeFramed(KAANA_FRAME_USAGE_REPORT, fixture.value);
      return { handled: true, value: frame.report };
    }
    case 'inferenceErrorSchema': {
      try {
        await decodeErrorBody(fixture.value);
      } catch (error) {
        if (error instanceof KaanaInferenceError) return { handled: true, value: error.failure };
        throw error;
      }
      throw new Error('unreachable');
    }
    default:
      if (fixture.schema in NOT_DECODED) return { handled: false };
      throw new Error(
        `no reading for ${fixture.schema}: add it to this script or to NOT_DECODED with a reason`,
      );
  }
}

const failures = [];
const valid = readFixtures('valid');
const invalid = readFixtures('invalid');

if (valid.length === 0) {
  throw new Error('no valid fixtures were found; every check below would pass vacuously');
}
if (invalid.length === 0) {
  throw new Error('no invalid control fixtures were found; the run would have no vacuity floor');
}

let decoded = 0;
let refused = 0;
const skippedSchemas = new Set();

for (const fixture of valid) {
  try {
    const result = await decode(fixture);
    if (!result.handled) {
      skippedSchemas.add(fixture.schema);
      continue;
    }
    decoded++;
    try {
      // Structural, not textual: zod rebuilds an object in the schema's
      // declaration order, so a string comparison would report every fixture as
      // corrupted while every value was intact.
      deepStrictEqual(result.value, fixture.value);
    } catch {
      failures.push(
        `DECODED but not faithfully — ${fixture.file} (${fixture.schema}/${fixture.case})\n` +
          `      in:  ${JSON.stringify(fixture.value)}\n      out: ${JSON.stringify(result.value)}`,
      );
    }
  } catch (error) {
    failures.push(
      `REFUSED a shape Relay produces — ${fixture.file} (${fixture.schema}/${fixture.case}): ${error.message}`,
    );
  }
}

for (const fixture of invalid) {
  let outcome;
  try {
    const result = await decode(fixture);
    outcome = result.handled ? 'accepted' : 'skipped';
  } catch (error) {
    outcome = error instanceof KaanaProtocolError ? 'refused' : `threw ${error.constructor.name}`;
  }
  if (outcome === 'skipped') {
    skippedSchemas.add(fixture.schema);
    continue;
  }
  if (outcome === 'refused') {
    refused++;
    continue;
  }
  failures.push(
    `did not refuse a control — ${fixture.file} (${fixture.schema}/${fixture.case}): ${outcome}. ` +
      'A shape the contract rejects must not reach a caller as an answer.',
  );
}

// The vacuity floors. A run that decoded nothing, or refused nothing, is
// indistinguishable from a clean one without these.
if (decoded === 0) {
  failures.push('no fixture was decoded; the client was never exercised');
}
if (refused === 0) {
  failures.push('no control was refused; the client would pass with every check disabled');
}

const expectedSkips = Object.keys(NOT_DECODED).sort().join(',');
const actualSkips = [...skippedSchemas].sort().join(',');
if (actualSkips !== expectedSkips) {
  failures.push(
    `the schemas this client does not read are [${actualSkips}]; NOT_DECODED names [${expectedSkips}]. ` +
      'Either the client gained a reading, or the contract gained a shape nobody routed.',
  );
}

process.stdout.write(
  `decoded ${decoded} produced shapes and refused ${refused} controls through the client; ` +
    `${skippedSchemas.size} schema(s) are not this client's to read\n`,
);

if (failures.length > 0) {
  process.stderr.write(`\n${failures.map((failure) => `  - ${failure}`).join('\n\n')}\n\n`);
  process.exit(1);
}
