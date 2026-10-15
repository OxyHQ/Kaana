/**
 * A bounded Server-Sent Events decoder over a fetch body.
 *
 * Multiple `data:` lines join with a newline, comments are ignored and a final
 * frame does not need a trailing blank line. Cancelling the iterator cancels
 * the body reader, which is how breaking out of `for await` closes the HTTP
 * request instead of leaving generation running upstream.
 */

import { KaanaProtocolError } from './errors.js';

/** One SSE frame. `event` is empty when the frame named none. */
export interface SseFrame {
  readonly event: string;
  readonly data: string;
}

/** The largest one frame's data may be: a 49 152-byte audio chunk in base64 plus JSON. */
const MAX_FRAME_CHARACTERS = 4 * 1024 * 1024;
const MAX_FRAME_DATA_LINES = 4096;

export async function* decodeEventStream(
  body: ReadableStream<Uint8Array>,
  requestId: string | null,
): AsyncGenerator<SseFrame> {
  const reader = body.getReader();
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let pending = '';
  let event = '';
  let data: string[] = [];
  let characters = 0;
  let finished = false;

  const decode = (chunk?: Uint8Array, options?: TextDecodeOptions): string => {
    try {
      return decoder.decode(chunk, options);
    } catch (_error) {
      throw new KaanaProtocolError('The stream is not valid UTF-8.', requestId);
    }
  };

  const dispatch = (): SseFrame | undefined => {
    if (data.length === 0 && event.length === 0) return undefined;
    const frame = { event, data: data.join('\n') };
    event = '';
    data = [];
    characters = 0;
    return frame;
  };

  const consume = (raw: string): SseFrame | undefined => {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    if (line.length === 0) return dispatch();
    if (line.startsWith(':')) return undefined;
    const separator = line.indexOf(':');
    const field = separator === -1 ? line : line.slice(0, separator);
    const rawValue = separator === -1 ? '' : line.slice(separator + 1);
    const value = rawValue.startsWith(' ') ? rawValue.slice(1) : rawValue;
    if (field === 'data') {
      characters += value.length;
      if (data.length >= MAX_FRAME_DATA_LINES || characters > MAX_FRAME_CHARACTERS) {
        throw new KaanaProtocolError('The stream sent a frame over its size limit.', requestId);
      }
      data.push(value);
    } else if (field === 'event') {
      event = value;
    }
    return undefined;
  };

  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      pending += decode(next.value, { stream: true });
      let newline = pending.indexOf('\n');
      while (newline !== -1) {
        const frame = consume(pending.slice(0, newline));
        pending = pending.slice(newline + 1);
        if (frame !== undefined) yield frame;
        newline = pending.indexOf('\n');
      }
      if (pending.length > MAX_FRAME_CHARACTERS) {
        throw new KaanaProtocolError('The stream sent a line over its size limit.', requestId);
      }
    }
    pending += decode();
    if (pending.length > 0) {
      const frame = consume(pending);
      if (frame !== undefined) yield frame;
    }
    const last = dispatch();
    if (last !== undefined) yield last;
    finished = true;
  } finally {
    if (!finished) await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}
