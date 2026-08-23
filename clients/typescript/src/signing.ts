/**
 * Ed25519 edge authentication — the SIGNING half of what `internal/edgeauth`
 * verifies.
 *
 * ## This file does not give Kaana the ability to mint an envelope
 *
 * `internal/edgeauth` states the asymmetry that makes this hop safe: Kaana holds
 * only PUBLIC keys and cannot construct an envelope it would itself accept,
 * because in any symmetric scheme verifying and minting are the same capability
 * — the key that lets a verifier READ an attribution is the key that lets it
 * FORGE one, and a forged `accountId` is indistinguishable from a real one at
 * every point after the mint.
 *
 * That asymmetry is about which PROCESS holds the private half, not about which
 * repository the code sits in. The Kaana binary is Go; it neither imports nor
 * can import this package, and no private key exists in this repository, in a
 * test or in CI — the tests generate an ephemeral keypair at runtime. The
 * private half lives only in Oxy, in the edge that mints envelopes. Publishing
 * the signer from here is what stops every Oxy caller writing its own fifth
 * version of the framing; it moves no key.
 *
 * ## The scheme
 *
 *     X-Oxy-Kaana-Key-Id       the signing key's id
 *     X-Oxy-Kaana-Timestamp    unix milliseconds, when Oxy signed
 *     X-Oxy-Kaana-Signature    v1=<base64 Ed25519 signature>
 *
 * signed over, `\n`-joined with no trailing newline:
 *
 *     oxy-relay-envelope:v1
 *     <key id>
 *     <timestamp>
 *     <lowercase hex sha256 of the exact request body bytes>
 *
 * The domain prefix means a signature minted for any other Oxy purpose cannot be
 * replayed as an inference envelope, and the body hash means the signature covers
 * the envelope rather than merely accompanying it.
 *
 * ## One `relay` spelling survives, and only one
 *
 * The HEADERS were renamed with the service. `internal/edgeauth` still accepts
 * `X-Oxy-Relay-*` as a fallback, but that fallback is a MIGRATION with an end —
 * its own comment says the three legacy constants are deleted once the edge
 * sends the new names. A client published from this repository emitting the old
 * ones would be the thing that stops that migration ending, so it emits the
 * current names and nothing else. The fallback is per header and the signature
 * covers the VALUES rather than the names they arrived in, so either spelling
 * verifies identically for as long as both are read.
 *
 * The DOMAIN SEPARATOR still says `relay`, deliberately and permanently: it is
 * inside the signed payload, so changing it changes every signature and would
 * need the edge to change in the same instant. Nothing outside these two files
 * ever sees it.
 *
 * ## Not a browser module
 *
 * It imports `node:crypto` and takes a private key. A signing key in a browser
 * bundle is a signing key the customer has.
 */

import {
  createHash,
  createPrivateKey,
  createPublicKey,
  sign as cryptoSign,
  type KeyObject,
} from 'node:crypto';

/** The three headers that carry the signature — `edgeauth.Header*`. */
export const EDGE_SIGNATURE_HEADERS = {
  keyId: 'X-Oxy-Kaana-Key-Id',
  timestamp: 'X-Oxy-Kaana-Timestamp',
  signature: 'X-Oxy-Kaana-Signature',
} as const;

/**
 * Domain separator: the first line of every signing input.
 *
 * It says `relay` after the rename because it is signed, not sent — see the
 * header note above.
 */
export const EDGE_SIGNATURE_DOMAIN = 'oxy-relay-envelope:v1';

/** Version marker on the signature header value. */
export const EDGE_SIGNATURE_PREFIX = 'v1=';

/**
 * How far Kaana lets a signature's timestamp drift, in either direction, before
 * it refuses the envelope — `edgeauth.DefaultMaxSkew`.
 *
 * It is the only replay bound, because Kaana keeps no nonce cache; a captured
 * envelope can be replayed inside this window, which is acceptable only because
 * the Oxy edge owns idempotency and spend reservation. Restated here so a caller
 * that queues or batches envelopes can see the bound it is working against
 * rather than discovering it as an authentication failure: signing happens at
 * send time, not at build time.
 */
export const EDGE_SIGNATURE_MAX_SKEW_MS = 5 * 60 * 1000;

/**
 * An Ed25519 private key with the id Kaana knows its public half by.
 *
 * The id is not a secret and travels in a header; the key is, and stays a
 * `KeyObject` so it is never a string this process can accidentally serialise.
 */
export interface KaanaSigningKey {
  readonly keyId: string;
  readonly privateKey: KeyObject;
}

/**
 * DER prefixes for a bare Ed25519 key.
 *
 * Both halves are configured as 32 raw bytes — `edgeauth.ParsePublicKeys` reads
 * `kid:base64,kid:base64` — but `node:crypto` imports only structured
 * encodings. These are the fixed PKCS#8 and SPKI headers for `id-Ed25519`
 * (RFC 8410), so wrapping is a byte concatenation rather than a dependency on an
 * ASN.1 encoder.
 */
const PKCS8_ED25519_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex');
const SPKI_ED25519_PREFIX = Buffer.from('302a300506032b6570032100', 'hex');

/** Ed25519 seeds and public keys are both exactly 32 bytes. */
const ED25519_KEY_BYTES = 32;

function decodeRawKey(material: string | Uint8Array, what: string): Buffer {
  const bytes =
    typeof material === 'string' ? Buffer.from(material, 'base64') : Buffer.from(material);
  if (bytes.length !== ED25519_KEY_BYTES) {
    // The length, never the value: a message quoting the material would put a
    // private key into whatever caught it.
    throw new TypeError(
      `the ${what} is ${bytes.length} bytes; an Ed25519 ${what} is ${ED25519_KEY_BYTES}`,
    );
  }
  return bytes;
}

/**
 * Builds a signing key from the 32-byte seed, base64 as it is stored.
 *
 * The seed IS the private key. Nothing derived from it leaves this function
 * except the opaque `KeyObject`.
 */
export function ed25519PrivateKeyFromSeed(seed: string | Uint8Array): KeyObject {
  return createPrivateKey({
    key: Buffer.concat([PKCS8_ED25519_PREFIX, decodeRawKey(seed, 'private key seed')]),
    format: 'der',
    type: 'pkcs8',
  });
}

/**
 * Builds a public key from its 32 raw bytes, base64 — the form
 * `edgeauth.ParsePublicKeys` is configured with.
 *
 * Public keys are not secrets: they are ordinary configuration and may appear in
 * a task definition, a workflow file or a log. This exists so a caller can prove
 * locally that its private key matches the public half Kaana was given, which is
 * otherwise discoverable only as a production authentication failure.
 */
export function ed25519PublicKeyFromRaw(raw: string | Uint8Array): KeyObject {
  return createPublicKey({
    key: Buffer.concat([SPKI_ED25519_PREFIX, decodeRawKey(raw, 'public key')]),
    format: 'der',
    type: 'spki',
  });
}

/** The 32 raw bytes of a public key, in the form Kaana is configured with. */
export function ed25519PublicKeyToRaw(key: KeyObject): Buffer {
  const spki = key.export({ format: 'der', type: 'spki' });
  return spki.subarray(spki.length - ED25519_KEY_BYTES);
}

/**
 * The exact bytes both sides sign — the TypeScript counterpart of
 * `edgeauth.SigningInput`.
 *
 * Exported because it IS the specification: a caller reproducing a signature, or
 * debugging a rejected envelope, reads this rather than re-deriving the framing
 * from prose.
 */
export function edgeSigningInput(
  keyId: string,
  timestampMillis: number,
  body: Uint8Array,
): Buffer {
  if (keyId.includes('\n') || keyId.includes('\r')) {
    // The signing input is line-separated, so a key id carrying a newline could
    // move the boundary between the id and the timestamp and let two different
    // (id, timestamp) pairs produce one preimage.
    throw new TypeError('a signing key id cannot contain a line break');
  }
  if (!Number.isSafeInteger(timestampMillis)) {
    throw new TypeError('the signing timestamp must be unix milliseconds as a safe integer');
  }
  const digest = createHash('sha256').update(body).digest('hex');
  return Buffer.from(
    [EDGE_SIGNATURE_DOMAIN, keyId, String(timestampMillis), digest].join('\n'),
    'utf8',
  );
}

/**
 * Signs `body` and returns the three headers to send with it.
 *
 * `body` must be the EXACT bytes that will be transmitted. `edgeauth.Verify`
 * checks the signature against the bytes it will parse, so signing a re-encoded
 * copy authenticates something other than what Kaana executes — which is the
 * classic way a signature check becomes decorative. {@link
 * serializeInferenceRequest} exists so a caller has one buffer to both sign and
 * send.
 */
export function signEdgeRequest(
  key: KaanaSigningKey,
  body: Uint8Array,
  timestampMillis: number,
): Record<string, string> {
  const signature = cryptoSign(
    // Ed25519 carries its own hash, so node requires the algorithm to be null.
    null,
    edgeSigningInput(key.keyId, timestampMillis, body),
    key.privateKey,
  );
  return {
    [EDGE_SIGNATURE_HEADERS.keyId]: key.keyId,
    [EDGE_SIGNATURE_HEADERS.timestamp]: String(timestampMillis),
    [EDGE_SIGNATURE_HEADERS.signature]: EDGE_SIGNATURE_PREFIX + signature.toString('base64'),
  };
}
