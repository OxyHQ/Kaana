/** `POST /v1/chat/completions`, the OpenAI dialect: whole and streamed. */

import {
  type ChatCompletion,
  type ChatCompletionChunk,
  type ChatCompletionUsage,
  chatCompletionChunkSchema,
  chatCompletionErrorSchema,
  chatCompletionSchema,
} from '@kaana.ai/contracts';
import { concatBytes, decodeBase64, encodeBase64 } from './base64.js';
import { isRetryableCode, kaanaError, KaanaProtocolError } from './errors.js';
import { cutOff, readFrames } from './events.js';
import { type RequestOptions, type Transport, validate } from './http.js';
import { type KaanaStream, StreamHandle } from './stream.js';

const DONE = '[DONE]';

export function chatStream(
  transport: Transport,
  body: string,
  options: RequestOptions,
): KaanaStream<ChatCompletionChunk, ChatCompletion> {
  return new StreamHandle<ChatCompletionChunk, ChatCompletion>(async function* (context) {
    const sent = await transport.send({
      method: 'POST',
      path: '/chat/completions',
      body,
      accept: 'text/event-stream',
      dialect: 'chat',
      options: { ...options, signal: context.signal },
      stream: true,
    });
    const requestId = sent.requestId;
    context.setRequestId(requestId);
    const fold = new ChatFold();
    let output = false;

    for await (const frame of readFrames(sent, () => output)) {
      if (frame.data === DONE) return fold.completion(requestId);
      let payload: unknown;
      try {
        payload = JSON.parse(frame.data);
      } catch (_error) {
        throw new KaanaProtocolError('The stream sent a frame that is not JSON.', requestId);
      }
      if (typeof payload === 'object' && payload !== null && 'error' in payload) {
        const { error } = validate(payload, chatCompletionErrorSchema, requestId, 'stream error');
        throw kaanaError({
          code: error.code,
          message: error.message,
          status: null,
          requestId,
          retryable: isRetryableCode(error.code),
          ...(error.param === null ? {} : { param: error.param }),
          afterOutput: output,
        });
      }
      const chunk = validate(payload, chatCompletionChunkSchema, requestId, 'stream chunk');
      if (chunk.choices.some((choice) => hasOutput(choice.delta))) {
        output = true;
        context.markOutput();
      }
      fold.add(chunk);
      yield chunk;
    }
    throw cutOff(requestId);
  }, options.signal);
}

function hasOutput(delta: ChatCompletionChunk['choices'][number]['delta']): boolean {
  return (
    (delta.content !== undefined && delta.content.length > 0) ||
    delta.refusal !== undefined ||
    delta.audio !== undefined ||
    (delta.tool_calls !== undefined && delta.tool_calls.length > 0)
  );
}

type Choice = ChatCompletion['choices'][number];

interface ChoiceFold {
  content: string | null;
  finishReason: string | null;
  audio?: { id: string; data: Uint8Array[]; transcript: string; expiresAt: number };
  toolCalls: Map<number, { id: string; name: string; arguments: string }>;
}

const ZERO_USAGE: ChatCompletionUsage = {
  prompt_tokens: 0,
  completion_tokens: 0,
  total_tokens: 0,
  prompt_tokens_details: { cached_tokens: 0 },
  completion_tokens_details: { reasoning_tokens: 0 },
};

/** Folds the chunks of one completion into the `ChatCompletion` it describes. */
export class ChatFold {
  #first: ChatCompletionChunk | undefined;
  #usage: ChatCompletionUsage | undefined;
  readonly #choices = new Map<number, ChoiceFold>();

  add(chunk: ChatCompletionChunk): void {
    this.#first ??= chunk;
    if (chunk.usage !== undefined) this.#usage = chunk.usage;
    for (const choice of chunk.choices) {
      const fold: ChoiceFold = this.#choices.get(choice.index) ?? {
        content: null,
        finishReason: null,
        toolCalls: new Map(),
      };
      const delta = choice.delta;
      if (delta.content !== undefined) fold.content = (fold.content ?? '') + delta.content;
      if (delta.audio !== undefined) {
        fold.audio ??= { id: delta.audio.id, data: [], transcript: '', expiresAt: 0 };
        if (delta.audio.data !== undefined) fold.audio.data.push(decodeBase64(delta.audio.data));
        if (delta.audio.transcript !== undefined) fold.audio.transcript += delta.audio.transcript;
        if (delta.audio.expires_at !== undefined) fold.audio.expiresAt = delta.audio.expires_at;
      }
      for (const call of delta.tool_calls ?? []) {
        const known = fold.toolCalls.get(call.index) ?? { id: '', name: '', arguments: '' };
        if (call.id !== undefined) known.id = call.id;
        if (call.function.name !== undefined) known.name += call.function.name;
        if (call.function.arguments !== undefined) known.arguments += call.function.arguments;
        fold.toolCalls.set(call.index, known);
      }
      if (choice.finish_reason !== null) fold.finishReason = choice.finish_reason;
      this.#choices.set(choice.index, fold);
    }
  }

  completion(requestId: string): ChatCompletion {
    const first = this.#first;
    if (first === undefined) {
      throw new KaanaProtocolError('The stream ended without a chunk.', requestId);
    }
    const choices: Choice[] = [...this.#choices]
      .sort(([a], [b]) => a - b)
      .map(([index, fold]) => {
        const message: Choice['message'] = { role: 'assistant', content: fold.content };
        if (fold.audio !== undefined) {
          message.audio = {
            id: fold.audio.id,
            data: encodeBase64(concatBytes(fold.audio.data)),
            transcript: fold.audio.transcript,
            expires_at: fold.audio.expiresAt,
          };
        }
        if (fold.toolCalls.size > 0) {
          message.tool_calls = [...fold.toolCalls]
            .sort(([a], [b]) => a - b)
            .map(([, call]) => ({
              id: call.id,
              type: 'function' as const,
              function: { name: call.name, arguments: call.arguments },
            }));
        }
        return { index, message, finish_reason: fold.finishReason ?? 'stop' };
      });
    return validate(
      {
        id: first.id,
        object: 'chat.completion',
        created: first.created,
        model: first.model,
        choices,
        usage: this.#usage ?? ZERO_USAGE,
      },
      chatCompletionSchema,
      requestId,
      'folded completion',
    );
  }
}
