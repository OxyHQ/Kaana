/** `POST /v1/responses`: whole, and streamed with a fold into the whole. */

import {
  type InferenceContentPart,
  type InferenceMessage,
  KAANA_ROUTING_POLICY_ID_HEADER,
  KAANA_ROUTING_POLICY_VERSION_HEADER,
  type Response,
  type ResponseStreamEvent,
  type RoutingPolicyReference,
  responseSchema,
  responseStreamEventSchema,
  routingPolicyReferenceSchema,
  type Usage,
} from '@kaana.ai/contracts';
import { concatBytes, decodeBase64, encodeBase64 } from './base64.js';
import { fromPublicError, KaanaProtocolError } from './errors.js';
import { cutOff, frameJson, readFrames } from './events.js';
import type { RequestOptions, Transport } from './http.js';
import { validate } from './http.js';
import { type KaanaStream, StreamHandle } from './stream.js';

export function responsesStream(
  transport: Transport,
  body: string,
  options: RequestOptions,
): KaanaStream<ResponseStreamEvent, Response> {
  return new StreamHandle<ResponseStreamEvent, Response>(async function* (context) {
    const sent = await transport.send({
      method: 'POST',
      path: '/responses',
      body,
      accept: 'text/event-stream',
      options: { ...options, signal: context.signal },
      stream: true,
    });
    const requestId = sent.requestId;
    context.setRequestId(requestId);
    const routingPolicy = routingPolicyFrom(sent.response, requestId);
    const fold = new ResponseFold();
    let lastSequence = -1;
    let output = false;

    for await (const frame of readFrames(sent, () => output)) {
      const event = validate(
        frameJson(frame, requestId),
        responseStreamEventSchema,
        requestId,
        'stream event',
      );
      if (frame.event !== event.type) {
        throw new KaanaProtocolError(
          `The stream framed a ${event.type} event as ${frame.event || 'unnamed'}.`,
          requestId,
        );
      }
      if (event.requestId !== requestId) {
        throw new KaanaProtocolError('The stream sent an event of another request.', requestId);
      }
      if (event.sequence <= lastSequence) {
        throw new KaanaProtocolError('The stream sequence did not advance.', requestId);
      }
      if (lastSequence === -1 && event.type !== 'start') {
        throw new KaanaProtocolError('The stream did not begin with start.', requestId);
      }
      lastSequence = event.sequence;
      if (event.type === 'delta' || event.type === 'audio' || event.type === 'tool_call') {
        output = true;
        context.markOutput();
      }
      fold.add(event);
      yield event;
      if (event.type === 'error') {
        throw fromPublicError(event.error, null, output);
      }
      if (event.type === 'done') {
        return fold.response(requestId, routingPolicy);
      }
    }
    throw cutOff(requestId);
  }, options.signal);
}

function routingPolicyFrom(
  response: globalThis.Response,
  requestId: string,
): RoutingPolicyReference {
  const routingPolicyId = response.headers.get(KAANA_ROUTING_POLICY_ID_HEADER);
  const version = response.headers.get(KAANA_ROUTING_POLICY_VERSION_HEADER);
  return validate(
    {
      routingPolicyId,
      policyVersion: version !== null && /^\d+$/.test(version) ? Number(version) : version,
    },
    routingPolicyReferenceSchema,
    requestId,
    `${KAANA_ROUTING_POLICY_ID_HEADER}/${KAANA_ROUTING_POLICY_VERSION_HEADER} headers`,
  );
}

/** Folds the events of one response into the `Response` it describes. */
export class ResponseFold {
  #start: Extract<ResponseStreamEvent, { type: 'start' }> | undefined;
  #done: Extract<ResponseStreamEvent, { type: 'done' }> | undefined;
  #text = '';
  #refusal = '';
  #audio: Uint8Array[] = [];
  #audioMediaType: string | undefined;
  #usage: Usage = [];
  readonly #toolCalls = new Map<string, { name: string; arguments: string }>();

  add(event: ResponseStreamEvent): void {
    switch (event.type) {
      case 'start':
        this.#start = event;
        break;
      case 'delta':
        if (event.channel === 'output_text' || event.channel === 'output_audio_transcript') {
          this.#text += event.text;
        } else if (event.channel === 'refusal') {
          this.#refusal += event.text;
        }
        break;
      case 'audio':
        this.#audioMediaType = event.mediaType;
        this.#audio.push(decodeBase64(event.data));
        break;
      case 'tool_call': {
        const call = this.#toolCalls.get(event.toolCallId) ?? { name: '', arguments: '' };
        if (event.name !== undefined) call.name = event.name;
        if (event.argumentsDelta !== undefined) call.arguments += event.argumentsDelta;
        this.#toolCalls.set(event.toolCallId, call);
        break;
      }
      case 'usage':
        this.#usage = [...event.units];
        break;
      case 'done':
        this.#done = event;
        break;
      case 'error':
        break;
    }
  }

  response(requestId: string, routingPolicy: RoutingPolicyReference): Response {
    const start = this.#start;
    const done = this.#done;
    if (start === undefined || done === undefined) {
      throw new KaanaProtocolError('The stream ended without start and done.', requestId);
    }
    const content: InferenceContentPart[] = [];
    if (this.#text.length > 0) content.push({ type: 'text', text: this.#text });
    if (this.#refusal.length > 0) content.push({ type: 'refusal', text: this.#refusal });
    if (this.#audio.length > 0 && this.#audioMediaType !== undefined) {
      content.push({
        type: 'audio',
        source: {
          kind: 'inline',
          mediaType: this.#audioMediaType,
          data: encodeBase64(concatBytes(this.#audio)),
        },
      });
    }
    const message: InferenceMessage = { role: 'assistant', content };
    if (this.#toolCalls.size > 0) {
      message.toolCalls = [...this.#toolCalls].map(([id, call]) => ({
        id,
        name: call.name,
        arguments: call.arguments,
      }));
    }
    const generationId = done.generationId ?? start.generationId;
    const latencyMs = Math.max(0, Date.parse(done.completedAt) - Date.parse(start.startedAt));
    return validate(
      {
        schemaVersion: 1,
        requestId,
        ...(generationId === undefined ? {} : { generationId }),
        model: start.model,
        finishReason: done.finishReason,
        output: [message],
        usage: this.#usage,
        routingPolicy,
        latencyMs: Number.isFinite(latencyMs) ? latencyMs : 0,
      },
      responseSchema,
      requestId,
      'folded response',
    );
  }
}
