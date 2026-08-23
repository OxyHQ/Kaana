/**
 * Reading Server-Sent Event frames off Kaana's response — the mirror of
 * `internal/sse`'s decoder, on the other side of the wire.
 *
 * It follows the SSE specification rather than the shape `internal/sse`'s WRITER
 * happens to emit today: multiple `data:` lines in one frame concatenate with a
 * newline, comment lines are ignored, a trailing frame with no blank line after
 * it still counts, and a frame may be split across any number of transport
 * chunks, including in the middle of a multi-byte character.
 *
 * Reading to the spec rather than to the producer matters because every one of
 * those cases is invisible in a test that feeds one whole response as one
 * string, and three of them — chunk splits, keep-alive comments, an
 * unterminated final frame — are produced by the network and the proxies in
 * front of it rather than by the producer at all.
 *
 * There is no `[DONE]` sentinel to look for. The contract's `done` and `error`
 * events are already terminal, and a second terminality signal is a second thing
 * that can disagree with the first.
 */

/** One decoded frame. */
export interface SseFrame {
  /** The `event:` field, empty when the producer sent none. */
  readonly name: string;
  /** The frame's `data:` lines, newline-joined. */
  readonly data: string;
}

/**
 * Decodes SSE frames from a stream of transport chunks.
 *
 * Takes an `AsyncIterable<Uint8Array>` rather than a `Response` so the chunk
 * boundaries — the part that is hard to get right and impossible to observe from
 * outside — can be chosen exactly by a test.
 */
export async function* readSseFrames(
  chunks: AsyncIterable<Uint8Array>,
): AsyncGenerator<SseFrame, void, undefined> {
  // `stream: true` keeps a multi-byte character straddling two chunks in the
  // decoder's own buffer instead of emitting a replacement character for each
  // half, which would corrupt the JSON payload rather than merely the text.
  const decoder = new TextDecoder('utf-8');
  let pending = '';
  let name = '';
  let data: string[] = [];

  function takeFrame(): SseFrame | null {
    // A blank line between two frames, or leading blank lines before the first,
    // dispatches nothing: a frame exists only once a field has been seen.
    if (data.length === 0 && name === '') return null;
    const frame: SseFrame = { name, data: data.join('\n') };
    name = '';
    data = [];
    return frame;
  }

  function consumeLine(rawLine: string): SseFrame | null {
    // Tolerate CRLF: a proxy that rewrites line endings must not merge fields.
    const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine;
    if (line === '') return takeFrame();
    // A comment, which is how a keep-alive is sent. Not a frame, and it must not
    // make the reader think a frame has started.
    if (line.startsWith(':')) return null;
    if (line.startsWith('data:')) {
      const value = line.slice('data:'.length);
      data.push(value.startsWith(' ') ? value.slice(1) : value);
      return null;
    }
    if (line.startsWith('event:')) {
      name = line.slice('event:'.length).trim();
      return null;
    }
    // `id:`, `retry:` and any field a future producer adds. Ignored rather than
    // refused: an unknown SSE field is transport, not contract.
    return null;
  }

  for await (const chunk of chunks) {
    pending += decoder.decode(chunk, { stream: true });
    let newline = pending.indexOf('\n');
    while (newline !== -1) {
      const frame = consumeLine(pending.slice(0, newline));
      pending = pending.slice(newline + 1);
      if (frame !== null) yield frame;
      newline = pending.indexOf('\n');
    }
  }

  pending += decoder.decode();
  if (pending !== '') {
    const frame = consumeLine(pending);
    if (frame !== null) yield frame;
  }
  // The producer was cut off, or simply sent no trailing blank line. What it did
  // send is still a frame, and for a usage report that is the difference between
  // settling a request and absorbing its cost.
  const trailing = takeFrame();
  if (trailing !== null) yield trailing;
}
