import { describe, expect, it } from 'bun:test';
import type { z } from 'zod';
import * as publicContract from '../index.js';
import * as internalContract from '../internal.js';
import {
  aliaProGrant,
  commercialGrant,
  generationCharged,
  generationMetered,
  model,
  publicError,
  requestId,
  settlement,
  speechModel,
  usage,
  usageReport,
} from './fixtures.js';

const at = '2026-10-15T09:41:00.000Z';

/**
 * One valid value per public endpoint shape and per grant/settlement shape.
 * Each must survive the wire: serialized to JSON and parsed back, it is equal to
 * what was sent, and parsing it again changes nothing.
 */
const cases: [keyof typeof publicContract | keyof typeof internalContract, unknown][] = [
  ['modelSchema', speechModel],
  ['modelListSchema', { data: [speechModel] }],
  [
    'powerLevelListSchema',
    {
      data: [
        { level: 'auto', modelLines: ['openai/gpt-5', 'openai/gpt-5-mini'] },
        {
          level: 'instant',
          powerClass: 'instant',
          reasoningEffort: 'low',
          modelLines: ['openai/gpt-5-mini'],
        },
      ],
    },
  ],
  [
    'responsesRequestSchema',
    {
      model: 'auto',
      input: [{ role: 'user', content: [{ type: 'text', text: 'Hola' }] }],
      stream: true,
      maxOutputTokens: 512,
      tools: [{ type: 'function', name: 'lookup', parameters: { type: 'object' } }],
      toolChoice: 'auto',
      reasoning: { effort: 'medium' },
      labels: { team: 'growth' },
    },
  ],
  ['responsesRequestSchema', { model, input: 'Hola' }],
  [
    'responseSchema',
    {
      schemaVersion: 1,
      requestId,
      generationId: 'gen_1',
      model,
      finishReason: 'stop',
      output: [{ role: 'assistant', content: [{ type: 'text', text: 'Hola.' }] }],
      usage,
      routingPolicy: { routingPolicyId: 'rp_1', policyVersion: 3 },
      latencyMs: 812,
    },
  ],
  [
    'responseStreamEventSchema',
    { schemaVersion: 1, type: 'start', requestId, sequence: 0, model, startedAt: at },
  ],
  [
    'responseStreamEventSchema',
    {
      schemaVersion: 1,
      type: 'delta',
      requestId,
      sequence: 1,
      outputIndex: 0,
      channel: 'output_text',
      text: 'Ho',
    },
  ],
  [
    'responseStreamEventSchema',
    {
      schemaVersion: 1,
      type: 'usage',
      requestId,
      sequence: 2,
      units: usage,
      usageSource: 'provider_reported',
    },
  ],
  [
    'responseStreamEventSchema',
    { schemaVersion: 1, type: 'error', requestId, sequence: 3, error: publicError },
  ],
  [
    'responseStreamEventSchema',
    {
      schemaVersion: 1,
      type: 'done',
      requestId,
      sequence: 4,
      finishReason: 'stop',
      completedAt: at,
    },
  ],
  [
    'chatCompletionsRequestSchema',
    {
      model: 'instant',
      messages: [
        { role: 'system', content: 'Be brief.' },
        { role: 'user', content: 'Hola' },
      ],
      stream: true,
      max_completion_tokens: 256,
      user: 'user_1',
    },
  ],
  [
    'chatCompletionSchema',
    {
      id: 'chatcmpl-1',
      object: 'chat.completion',
      created: 1760521260,
      model,
      choices: [
        { index: 0, message: { role: 'assistant', content: 'Hola.' }, finish_reason: 'stop' },
      ],
      usage: {
        prompt_tokens: 10,
        completion_tokens: 3,
        total_tokens: 13,
        prompt_tokens_details: { cached_tokens: 0 },
        completion_tokens_details: { reasoning_tokens: 0 },
      },
    },
  ],
  [
    'chatCompletionChunkSchema',
    {
      id: 'chatcmpl-1',
      object: 'chat.completion.chunk',
      created: 1760521260,
      model,
      choices: [{ index: 0, delta: { role: 'assistant', content: 'Ho' }, finish_reason: null }],
    },
  ],
  [
    'chatCompletionErrorSchema',
    {
      error: { message: 'Slow down.', type: 'rate_limit_error', param: null, code: 'rate_limited' },
    },
  ],
  ['embeddingsRequestSchema', { model: 'openai/text-embedding-3-small', input: ['a', 'b'] }],
  [
    'embeddingsResponseSchema',
    {
      object: 'list',
      data: [
        { object: 'embedding', index: 0, embedding: [0.1, -0.2] },
        { object: 'embedding', index: 1, embedding: [0.3, 0.4] },
      ],
      model: 'openai/text-embedding-3-small@2024-01-25',
      usage: { prompt_tokens: 2, total_tokens: 2 },
    },
  ],
  [
    'speechRequestSchema',
    {
      model: 'elevenlabs/eleven-multilingual',
      input: 'Hola',
      voice: 'rachel',
      response_format: 'mp3',
      speed: 1.1,
      stream: true,
      stream_format: 'sse',
    },
  ],
  ['speechStreamEventSchema', { type: 'speech.audio.delta', audio: 'SUQzBAAAAAAA' }],
  [
    'speechStreamEventSchema',
    {
      type: 'speech.audio.done',
      requestId,
      model: 'elevenlabs/eleven-multilingual@v2',
      usage: [{ unit: 'characters', quantity: 4 }],
    },
  ],
  ['speechStreamEventSchema', { type: 'error', error: publicError }],
  [
    'transcriptionRequestSchema',
    { model: 'openai/whisper-1', language: 'es-ES', timestamps: 'segment' },
  ],
  [
    'transcriptionJsonRequestSchema',
    {
      model: 'openai/whisper-1',
      language: 'es-ES',
      audio: { mediaType: 'audio/wav', data: 'UklGRiQAAABXQVZF' },
    },
  ],
  [
    'transcriptionSchema',
    {
      schemaVersion: 1,
      requestId,
      model: 'openai/whisper-1@2023-03-01',
      text: 'Hola mundo',
      language: 'es-ES',
      durationMs: 1200,
      segments: [{ startMs: 0, endMs: 1200, text: 'Hola mundo' }],
      usage: [{ unit: 'audio_input_milliseconds', quantity: 1200 }],
    },
  ],
  [
    'imageGenerationsRequestSchema',
    { model: 'openai/gpt-image-1@2025-04-15', prompt: 'A fox', n: 2 },
  ],
  ['imageGenerationsSchema', { created: 1760521260, data: [{ url: 'https://kaana.ai/i/1' }] }],
  [
    'decisionRequestSchema',
    {
      model,
      state: 'A post about cats.',
      questions: [{ id: 'q1', kind: 'choice', question: 'Topic?', options: ['pets', 'cars'] }],
    },
  ],
  ['generationRecordSchema', generationCharged],
  ['generationRecordSchema', generationMetered],
  ['publicErrorSchema', publicError],
  [
    'realtimeSessionOpenSchema',
    {
      type: 'session.open',
      kind: 'transcription',
      config: {
        inputAudioFormat: 'pcm16_24khz',
        turnDetection: { type: 'none' },
        inputAudioTranscription: { language: 'es' },
      },
      limits: { maxDurationMs: 60_000, maxTextItems: 0 },
    },
  ],
  [
    'realtimePublicServerEventSchema',
    {
      schemaVersion: 1,
      requestId,
      sequence: 9,
      type: 'session.closed',
      reason: 'client_closed',
      units: [{ unit: 'audio_input_milliseconds', quantity: 1000 }],
      usageSource: 'provider_reported',
      closedAt: at,
    },
  ],
  ['grantSchema', commercialGrant],
  ['grantSchema', aliaProGrant],
  ['settlementSchema', settlement],
  ['normalizedUsageReportSchema', usageReport],
];

const schemaNamed = (name: string): z.ZodType => {
  const schema =
    (publicContract as Record<string, unknown>)[name] ??
    (internalContract as Record<string, unknown>)[name];
  if (schema === undefined) throw new Error(`no schema ${name}`);
  return schema as z.ZodType;
};

describe('round trip', () => {
  it.each(cases.map(([name, value], index) => [`${index}:${String(name)}`, name, value] as const))(
    '%s survives JSON and a second parse unchanged',
    (_label, name, value) => {
      const schema = schemaNamed(String(name));
      const wire = JSON.parse(JSON.stringify(value));
      const parsed = schema.safeParse(wire);
      if (!parsed.success) throw new Error(JSON.stringify(parsed.error.issues, null, 2));
      expect(parsed.data).toEqual(wire);
      expect(schema.parse(JSON.parse(JSON.stringify(parsed.data)))).toEqual(parsed.data);
    },
  );

  it('covers every public endpoint body', () => {
    const covered = new Set(cases.map(([name]) => name));
    for (const name of [
      'modelSchema',
      'modelListSchema',
      'powerLevelListSchema',
      'responsesRequestSchema',
      'responseSchema',
      'responseStreamEventSchema',
      'chatCompletionsRequestSchema',
      'chatCompletionSchema',
      'chatCompletionChunkSchema',
      'chatCompletionErrorSchema',
      'embeddingsRequestSchema',
      'embeddingsResponseSchema',
      'speechRequestSchema',
      'speechStreamEventSchema',
      'transcriptionRequestSchema',
      'transcriptionJsonRequestSchema',
      'transcriptionSchema',
      'imageGenerationsRequestSchema',
      'imageGenerationsSchema',
      'decisionRequestSchema',
      'generationRecordSchema',
      'publicErrorSchema',
      'realtimeSessionOpenSchema',
      'realtimePublicServerEventSchema',
    ] as const) {
      expect(covered.has(name)).toBe(true);
    }
  });

  it('refuses an unknown field on a public request rather than ignoring it', () => {
    expect(
      publicContract.responsesRequestSchema.safeParse({ model, input: 'Hola', logprobs: true })
        .success,
    ).toBe(false);
    expect(
      publicContract.speechRequestSchema.safeParse({
        model: 'elevenlabs/eleven-multilingual',
        input: 'Hola',
        voice: 'rachel',
        instructions: 'whisper',
      }).success,
    ).toBe(false);
  });

  it('names a model reference or a power level, nothing else', () => {
    const target = publicContract.modelTargetSchema;
    expect(target.safeParse('openai/gpt-5').success).toBe(true);
    expect(target.safeParse('openai/gpt-5@2026-06-01').success).toBe(true);
    expect(target.safeParse('ultra').success).toBe(true);
    expect(target.safeParse('gpt-4o').success).toBe(false);
    expect(target.safeParse('rpf_01JQZEXACT').success).toBe(false);
  });
});
