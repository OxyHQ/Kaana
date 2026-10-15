/**
 * Base64 without a platform module: no `Buffer`, no `btoa`/`atob` (which are
 * Latin-1 string APIs and absent from some React Native runtimes). The SDK
 * encodes audio frames and credentials with these and nothing else.
 */

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

const DECODE = (() => {
  const table = new Int16Array(128).fill(-1);
  for (let index = 0; index < ALPHABET.length; index += 1) {
    table[ALPHABET.charCodeAt(index)] = index;
  }
  // base64url spellings decode to the same values.
  table['-'.charCodeAt(0)] = 62;
  table['_'.charCodeAt(0)] = 63;
  return table;
})();

/** Standard padded base64 (RFC 4648 §4). */
export function encodeBase64(bytes: Uint8Array): string {
  let out = '';
  let index = 0;
  for (; index + 2 < bytes.length; index += 3) {
    const chunk = (bytes[index] << 16) | (bytes[index + 1] << 8) | bytes[index + 2];
    out +=
      ALPHABET[(chunk >> 18) & 63] +
      ALPHABET[(chunk >> 12) & 63] +
      ALPHABET[(chunk >> 6) & 63] +
      ALPHABET[chunk & 63];
  }
  const rest = bytes.length - index;
  if (rest === 1) {
    const chunk = bytes[index] << 16;
    out += `${ALPHABET[(chunk >> 18) & 63]}${ALPHABET[(chunk >> 12) & 63]}==`;
  } else if (rest === 2) {
    const chunk = (bytes[index] << 16) | (bytes[index + 1] << 8);
    out += `${ALPHABET[(chunk >> 18) & 63]}${ALPHABET[(chunk >> 12) & 63]}${ALPHABET[(chunk >> 6) & 63]}=`;
  }
  return out;
}

/** Unpadded base64url (RFC 4648 §5), the form a WebSocket subprotocol token can carry. */
export function encodeBase64Url(bytes: Uint8Array): string {
  return encodeBase64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * Decode standard or url-safe base64, padded or not. Throws on any character
 * outside the alphabet or an impossible length, never silently skipping.
 */
export function decodeBase64(text: string): Uint8Array {
  const trimmed = text.replace(/=+$/, '');
  if (trimmed.length % 4 === 1) throw new Error('base64 has an impossible length');
  const out = new Uint8Array(Math.floor((trimmed.length * 3) / 4));
  let buffer = 0;
  let bits = 0;
  let position = 0;
  for (let index = 0; index < trimmed.length; index += 1) {
    const code = trimmed.charCodeAt(index);
    const value = code < 128 ? DECODE[code] : -1;
    if (value < 0) throw new Error('base64 contains a character outside its alphabet');
    buffer = (buffer << 6) | value;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[position] = (buffer >> bits) & 0xff;
      position += 1;
    }
  }
  return out;
}

/** UTF-8 bytes of a string. */
export function utf8(text: string): Uint8Array {
  return new TextEncoder().encode(text);
}

/** Concatenate byte chunks. */
export function concatBytes(chunks: readonly Uint8Array[]): Uint8Array {
  let size = 0;
  for (const chunk of chunks) size += chunk.byteLength;
  const out = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}
