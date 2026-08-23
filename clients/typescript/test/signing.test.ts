/**
 * The signature is checked the way `internal/edgeauth` checks it — with the
 * PUBLIC half, against a preimage this file spells out from the specification in
 * its own bytes.
 *
 * Not against a recorded signature string. A golden string pins whatever the
 * implementation produced on the day it was recorded, including a wrong
 * separator or a missing domain line, and re-recording it when it changes is the
 * whole failure mode. Verifying with the public key is the same operation the
 * data plane performs, so a preimage this side gets wrong fails here for exactly
 * the reason it would fail in production.
 *
 * Every positive case is paired with a negative that would pass if the preimage
 * carried less than it does: a different body, a different domain, a different
 * timestamp, a different key id. That pairing is the positive control — "the
 * signature verified" is also what a verifier that ignores its input reports.
 *
 * No key material is committed. Every keypair below is generated at runtime.
 */

import {
  createHash,
  generateKeyPairSync,
  verify as cryptoVerify,
  type KeyObject,
} from 'node:crypto';
import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  ed25519PrivateKeyFromSeed,
  ed25519PublicKeyFromRaw,
  ed25519PublicKeyToRaw,
  edgeSigningInput,
  signEdgeRequest,
  EDGE_SIGNATURE_HEADERS,
  EDGE_SIGNATURE_PREFIX,
  type KaanaSigningKey,
} from '../src/signing.js';

/**
 * The signing input, spelled out from the specification rather than built with
 * the function under test.
 *
 * The duplication is the point: a test calling `edgeSigningInput` on both sides
 * would verify the implementation against itself and pass with any framing at
 * all.
 */
function specSigningInput(keyId: string, timestampMillis: number, body: Uint8Array): Buffer {
  const digest = createHash('sha256').update(body).digest('hex');
  return Buffer.from(`oxy-relay-envelope:v1\n${keyId}\n${timestampMillis}\n${digest}`, 'utf8');
}

/** Exactly what `edgeauth.Verify` does with the three headers. */
function verifyLikeKaana(
  headers: Record<string, string>,
  body: Uint8Array,
  publicKey: KeyObject,
): boolean {
  const keyId = headers[EDGE_SIGNATURE_HEADERS.keyId] ?? '';
  const timestamp = Number(headers[EDGE_SIGNATURE_HEADERS.timestamp]);
  const raw = headers[EDGE_SIGNATURE_HEADERS.signature] ?? '';
  if (!raw.startsWith(EDGE_SIGNATURE_PREFIX)) return false;
  const signature = Buffer.from(raw.slice(EDGE_SIGNATURE_PREFIX.length), 'base64');
  return cryptoVerify(null, specSigningInput(keyId, timestamp, body), publicKey, signature);
}

function keypair(): { key: KaanaSigningKey; publicKey: KeyObject; rawPublicKey: Buffer } {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  // The 32-byte seed is the tail of the PKCS#8 DER and the raw public key the
  // tail of the SPKI. Taking them apart and putting them back through this
  // package's own importers is what proves those importers accept the form Oxy
  // and `edgeauth.ParsePublicKeys` actually store.
  const seed = privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32);
  const rawPublicKey = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  return {
    key: { keyId: 'edge-2026-08', privateKey: ed25519PrivateKeyFromSeed(seed.toString('base64')) },
    publicKey: ed25519PublicKeyFromRaw(rawPublicKey.toString('base64')),
    rawPublicKey,
  };
}

const BODY = new TextEncoder().encode('{"schemaVersion":1,"hello":"world"}');
const TIMESTAMP = 1_755_990_000_123;

describe('the edge signature', () => {
  it('verifies with the public half, against a preimage built from the spec', () => {
    const { key, publicKey } = keypair();

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);

    assert.equal(verifyLikeKaana(headers, BODY, publicKey), true);
  });

  it('does not verify over a different body', () => {
    const { key, publicKey } = keypair();
    const tampered = new TextEncoder().encode('{"schemaVersion":1,"hello":"worlds"}');

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);

    assert.equal(verifyLikeKaana(headers, tampered, publicKey), false);
  });

  it('does not verify under a different key', () => {
    const { key } = keypair();
    const other = keypair();

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);

    assert.equal(verifyLikeKaana(headers, BODY, other.publicKey), false);
  });

  it('covers the timestamp, so a captured signature cannot be re-stamped', () => {
    const { key, publicKey } = keypair();

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);
    const restamped = {
      ...headers,
      [EDGE_SIGNATURE_HEADERS.timestamp]: String(TIMESTAMP + 1000),
    };

    assert.equal(verifyLikeKaana(restamped, BODY, publicKey), false);
  });

  it('covers the key id, so a signature cannot be re-attributed', () => {
    const { key, publicKey } = keypair();

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);
    const reattributed = { ...headers, [EDGE_SIGNATURE_HEADERS.keyId]: 'edge-2026-09' };

    assert.equal(verifyLikeKaana(reattributed, BODY, publicKey), false);
  });

  it('carries the domain separator, so another Oxy signature cannot be replayed here', () => {
    const { key, publicKey } = keypair();

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);
    const signature = Buffer.from(
      (headers[EDGE_SIGNATURE_HEADERS.signature] ?? '').slice(EDGE_SIGNATURE_PREFIX.length),
      'base64',
    );
    const digest = createHash('sha256').update(BODY).digest('hex');
    const withoutDomain = Buffer.from(`${key.keyId}\n${TIMESTAMP}\n${digest}`, 'utf8');
    const otherDomain = Buffer.from(
      `oxy-relay-envelope:v2\n${key.keyId}\n${TIMESTAMP}\n${digest}`,
      'utf8',
    );

    assert.equal(cryptoVerify(null, withoutDomain, publicKey, signature), false);
    assert.equal(cryptoVerify(null, otherDomain, publicKey, signature), false);
  });

  it('names the headers edgeauth reads, and versions the signature value', () => {
    const { key } = keypair();

    const headers = signEdgeRequest(key, BODY, TIMESTAMP);

    // The CURRENT names, spelled out rather than read from the constant: a test
    // that asserted `headers[EDGE_SIGNATURE_HEADERS.keyId]` would keep passing
    // through a rename in either direction, and this client emitting the legacy
    // `X-Oxy-Relay-*` is exactly what would keep edgeauth's migration from
    // ending.
    assert.deepEqual(Object.keys(headers).sort(), [
      'X-Oxy-Kaana-Key-Id',
      'X-Oxy-Kaana-Signature',
      'X-Oxy-Kaana-Timestamp',
    ]);
    assert.equal(headers['X-Oxy-Kaana-Key-Id'], 'edge-2026-08');
    assert.equal(headers['X-Oxy-Kaana-Timestamp'], String(TIMESTAMP));
    assert.match(headers['X-Oxy-Kaana-Signature'] ?? '', /^v1=/);
    assert.equal(
      Buffer.from((headers['X-Oxy-Kaana-Signature'] ?? '').slice(3), 'base64').length,
      64,
    );
  });

  it('signs an empty body, which is what the health route verifies', () => {
    const { key, publicKey } = keypair();

    const headers = signEdgeRequest(key, new Uint8Array(0), TIMESTAMP);

    assert.equal(verifyLikeKaana(headers, new Uint8Array(0), publicKey), true);
    assert.equal(verifyLikeKaana(headers, BODY, publicKey), false);
  });
});

describe('edgeSigningInput', () => {
  it('produces exactly the four lines the specification names', () => {
    const input = edgeSigningInput('edge-2026-08', TIMESTAMP, BODY).toString('utf8');

    assert.deepEqual(input.split('\n'), [
      'oxy-relay-envelope:v1',
      'edge-2026-08',
      String(TIMESTAMP),
      createHash('sha256').update(BODY).digest('hex'),
    ]);
    assert.equal(input.endsWith('\n'), false);
  });

  it('refuses a key id carrying a line break, which would move a field boundary', () => {
    assert.throws(() => edgeSigningInput('edge\n2026', TIMESTAMP, BODY), TypeError);
    assert.throws(() => edgeSigningInput('edge\r2026', TIMESTAMP, BODY), TypeError);
  });

  it('refuses a timestamp that is not unix milliseconds', () => {
    assert.throws(() => edgeSigningInput('edge-2026-08', 1.5, BODY), TypeError);
    assert.throws(() => edgeSigningInput('edge-2026-08', Number.NaN, BODY), TypeError);
  });
});

describe('key material', () => {
  it('round-trips a public key through the form edgeauth is configured with', () => {
    const { publicKey, rawPublicKey } = keypair();

    assert.equal(
      ed25519PublicKeyToRaw(publicKey).toString('base64'),
      rawPublicKey.toString('base64'),
    );
  });

  it('refuses key material of the wrong length without quoting it', () => {
    const seed = Buffer.alloc(31, 7).toString('base64');

    let thrown: Error | null = null;
    try {
      ed25519PrivateKeyFromSeed(seed);
    } catch (error) {
      thrown = error as Error;
    }

    assert.match(thrown?.message ?? '', /31 bytes/);
    // The material must not survive into anything that catches this.
    assert.equal((thrown?.message ?? '').includes(seed), false);
  });
});
