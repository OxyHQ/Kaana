/** Reading a `text/event-stream` response into validated frames. */

import { cancelledByCaller, KaanaConnectionError, KaanaProtocolError } from './errors.js';
import { bodyReadFailure, mediaTypeOf, type Sent } from './http.js';
import { decodeEventStream, type SseFrame } from './sse.js';

/** The response body as a stream, or a clear failure where the runtime has none. */
export function streamingBody(sent: Sent): ReadableStream<Uint8Array> {
  const body = sent.response.body;
  if (body === null || typeof body.getReader !== 'function') {
    sent.abort();
    sent.release();
    throw new KaanaConnectionError(
      "This runtime's fetch exposes no streaming response body, which streaming methods need. " +
        'In React Native, install a fetch that streams (for example a streaming fetch polyfill); ' +
        'the non-streaming methods work without one.',
    );
  }
  return body;
}

export async function* readFrames(sent: Sent, output: () => boolean): AsyncGenerator<SseFrame> {
  if (mediaTypeOf(sent.response) !== 'text/event-stream') {
    sent.abort();
    sent.release();
    throw new KaanaProtocolError(
      `A streaming request was answered with ${mediaTypeOf(sent.response) ?? 'no Content-Type'} instead of text/event-stream.`,
      sent.requestId,
    );
  }
  const body = streamingBody(sent);
  try {
    for await (const frame of decodeEventStream(body, sent.requestId)) yield frame;
  } catch (error) {
    if (error instanceof KaanaProtocolError) throw error;
    if (sent.aborted()) throw cancelledByCaller(sent.requestId, output());
    throw bodyReadFailure(sent, error);
  } finally {
    sent.release();
  }
}

/** Parse one frame's JSON. */
export function frameJson(frame: SseFrame, requestId: string): unknown {
  try {
    return JSON.parse(frame.data);
  } catch (_error) {
    throw new KaanaProtocolError(
      `The stream sent a ${frame.event || 'unnamed'} frame that is not JSON.`,
      requestId,
    );
  }
}

/** A stream that ended without its terminal event was cut off. */
export function cutOff(requestId: string): KaanaConnectionError {
  return new KaanaConnectionError(
    `The stream of request ${requestId} ended before its terminal event.`,
  );
}
