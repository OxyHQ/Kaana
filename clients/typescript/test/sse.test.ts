/**
 * SSE reading, exercised at the boundaries the network produces rather than the
 * ones `internal/sse`'s writer emits.
 *
 * The chunk-splitting cases are the load-bearing ones: a reader that only ever
 * sees one whole response as one chunk passes every case in this file except
 * those, and fails in production the first time a frame straddles a TCP
 * boundary. The one-byte-at-a-time case is the positive control for all of them
 * — a correct reader must produce exactly the same frames as the single-chunk
 * feed, and if it is not correct no other case here would say so.
 */

import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { readSseFrames, type SseFrame } from '../src/sse.js';

const encoder = new TextEncoder();

async function* chunksOf(...chunks: readonly (string | Uint8Array)[]): AsyncGenerator<Uint8Array> {
  for (const chunk of chunks) {
    yield typeof chunk === 'string' ? encoder.encode(chunk) : chunk;
  }
}

async function collect(chunks: AsyncIterable<Uint8Array>): Promise<SseFrame[]> {
  const frames: SseFrame[] = [];
  for await (const frame of readSseFrames(chunks)) frames.push(frame);
  return frames;
}

/** Every byte on its own, the harshest split a transport can produce. */
async function* byteAtATime(text: string): AsyncGenerator<Uint8Array> {
  for (const byte of encoder.encode(text)) yield Uint8Array.of(byte);
}

describe('readSseFrames', () => {
  it('reads a named frame with its payload', async () => {
    const frames = await collect(chunksOf('event: stream_event\ndata: {"type":"delta"}\n\n'));

    assert.deepEqual(frames, [{ name: 'stream_event', data: '{"type":"delta"}' }]);
  });

  it('concatenates multiple data lines with a newline', async () => {
    const frames = await collect(chunksOf('event: e\ndata: one\ndata: two\ndata: three\n\n'));

    assert.deepEqual(frames, [{ name: 'e', data: 'one\ntwo\nthree' }]);
  });

  it('strips exactly one leading space after the colon, and no more', async () => {
    const frames = await collect(chunksOf('data:  padded\n\ndata:tight\n\n'));

    assert.deepEqual(
      frames.map((frame) => frame.data),
      [' padded', 'tight'],
    );
  });

  it('ignores comment lines, and a comment alone starts no frame', async () => {
    const frames = await collect(chunksOf(': keep-alive\n\n: another\nevent: e\ndata: x\n\n'));

    assert.deepEqual(frames, [{ name: 'e', data: 'x' }]);
  });

  it('ignores id and retry fields', async () => {
    const frames = await collect(chunksOf('id: 7\nretry: 3000\nevent: e\ndata: x\n\n'));

    assert.deepEqual(frames, [{ name: 'e', data: 'x' }]);
  });

  it('yields a trailing frame the producer left unterminated', async () => {
    const frames = await collect(chunksOf('event: usage_report\ndata: {"units":[]}'));

    assert.deepEqual(frames, [{ name: 'usage_report', data: '{"units":[]}' }]);
  });

  it('tolerates CRLF line endings', async () => {
    const frames = await collect(chunksOf('event: e\r\ndata: x\r\ndata: y\r\n\r\n'));

    assert.deepEqual(frames, [{ name: 'e', data: 'x\ny' }]);
  });

  it('emits nothing for blank lines alone', async () => {
    const frames = await collect(chunksOf('\n\n\n\n'));

    assert.deepEqual(frames, []);
  });

  it('reads frames split across chunk boundaries', async () => {
    const frames = await collect(
      chunksOf('event: stre', 'am_event\ndat', 'a: {"a":1,', '"b":2}\n', '\nevent: x\ndata: y\n\n'),
    );

    assert.deepEqual(frames, [
      { name: 'stream_event', data: '{"a":1,"b":2}' },
      { name: 'x', data: 'y' },
    ]);
  });

  it('produces the same frames one byte at a time as in one chunk', async () => {
    const wire =
      ': hello\n' +
      'event: stream_event\ndata: {"type":"start","requestId":"req_1"}\n\n' +
      'event: stream_event\ndata: {"type":"delta"}\ndata: {"continued":true}\n\n' +
      'event: usage_report\ndata: {"units":[]}\n\n';

    const whole = await collect(chunksOf(wire));
    const dribbled = await collect(byteAtATime(wire));

    assert.deepEqual(dribbled, whole);
    assert.equal(whole.length, 3);
  });

  it('keeps a multi-byte character whole when it straddles two chunks', async () => {
    // "Hola ✅" — the check mark is three bytes, split down the middle here. A
    // decoder without streaming state emits a replacement character for each
    // half, which corrupts the JSON rather than merely the text.
    const payload = encoder.encode('event: e\ndata: {"text":"Hola ✅"}\n\n');
    const cut = payload.indexOf(0xe2) + 1;

    const frames = await collect(chunksOf(payload.subarray(0, cut), payload.subarray(cut)));

    assert.deepEqual(frames, [{ name: 'e', data: '{"text":"Hola ✅"}' }]);
  });

  it('keeps a colon inside a payload out of the field parsing', async () => {
    const frames = await collect(chunksOf('data: {"url":"https://relay.oxy.so/x"}\n\n'));

    assert.deepEqual(frames, [{ name: '', data: '{"url":"https://relay.oxy.so/x"}' }]);
  });

  it('reads an empty stream as no frames', async () => {
    const frames = await collect(chunksOf());

    assert.deepEqual(frames, []);
  });
});
