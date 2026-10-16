/**
 * Contract set 4.1.0 (Alia ADR 0017, Kaana phase 1): input transcription inside
 * a realtime conversation — its usage unit, the catalogue flag that declares
 * it, the realtime audio capabilities a model publishes, and the check a
 * session open is held to before a grant is asked for.
 */
import { describe, expect, it } from 'bun:test';
import {
  checkRealtimeSessionOpen,
  INFERENCE_CONTRACT_VERSION,
  type ModelAudioCapabilities,
  type ModelCapabilities,
  modelAudioCapabilitiesSchema,
  modelCapabilitiesSchema,
  modelRealtimeCapabilitiesSchema,
  realtimeAudioFormatSchema,
  realtimePublicServerEventSchema,
  realtimeSessionOpenSchema,
  USAGE_UNITS,
  usageUnitSchema,
} from '../index.js';
import { normalizedUsageReportSchema, realtimeServerEventSchema } from '../internal.js';
import { requestId, usageReport } from './fixtures.js';

const realtime = {
  transports: ['websocket'],
  sessionKinds: ['conversation'],
  inputAudioTranscription: true,
  audioFormats: {
    input: ['pcm16_24khz', 'g711_ulaw', 'g711_alaw'],
    output: ['pcm16_24khz', 'g711_ulaw', 'g711_alaw'],
  },
} as const;

const voiceModel: ModelCapabilities = modelCapabilitiesSchema.parse({
  inputModalities: ['audio', 'text'],
  outputModalities: ['audio', 'text'],
  tools: true,
  parallelToolCalls: false,
  structuredOutput: false,
  jsonMode: false,
  reasoning: false,
  streaming: true,
  promptCaching: false,
  maxContextTokens: 128_000,
  maxOutputTokens: 4096,
  realtime,
});

const voiceAudio: ModelAudioCapabilities = modelAudioCapabilitiesSchema.parse({
  voices: [
    { id: 'eve', name: 'Eve', languages: ['en', 'es-ES', 'es-MX'], gender: 'female' },
    { id: 'rex', name: 'Rex', languages: ['en', 'es-ES', 'es-MX'], gender: 'male' },
  ],
  languages: ['en', 'es-ES', 'es-MX'],
  audioFormats: { input: [], output: [] },
});

const open = (config: Record<string, unknown> = {}, fields: Record<string, unknown> = {}) =>
  realtimeSessionOpenSchema.parse({
    type: 'session.open',
    kind: 'conversation',
    config: {
      outputModalities: ['audio'],
      voice: 'eve',
      inputAudioFormat: 'pcm16_24khz',
      outputAudioFormat: 'pcm16_24khz',
      turnDetection: { type: 'server_vad', createResponse: true, interruptResponse: true },
      inputAudioTranscription: { language: 'es-ES' },
      ...config,
    },
    ...fields,
  });

describe('contract set 4.1.0', () => {
  it('is in the set this package declares (4.1.0 or later)', () => {
    const [major = 0, minor = 0] = INFERENCE_CONTRACT_VERSION.split('.').map(Number);
    expect(major > 4 || (major === 4 && minor >= 1)).toBe(true);
  });
});

describe('input_transcription_milliseconds', () => {
  it('is a unit of its own, beside the session units rather than instead of them', () => {
    expect(USAGE_UNITS).toContain('input_transcription_milliseconds');
    expect(usageUnitSchema.safeParse('input_transcription_milliseconds').success).toBe(true);
    // Negative control: a near-miss spelling stays outside the closed set.
    expect(usageUnitSchema.safeParse('input_transcription_seconds').success).toBe(false);
  });

  it("settles beside the session model's own audio tokens: two models, two charges", () => {
    const units = [
      { unit: 'audio_input_tokens', quantity: 1_200 },
      { unit: 'audio_output_tokens', quantity: 2_400 },
      { unit: 'input_transcription_milliseconds', quantity: 12_500 },
    ];
    expect(
      realtimeServerEventSchema.safeParse({
        schemaVersion: 1,
        requestId,
        sequence: 9,
        type: 'session.closed',
        reason: 'client_closed',
        deploymentId: 'dep_openai_realtime_1',
        units,
        usageSource: 'provider_reported',
        closedAt: '2026-10-15T10:00:00.000Z',
      }).success,
    ).toBe(true);
    expect(normalizedUsageReportSchema.safeParse({ ...usageReport, units }).success).toBe(true);
  });

  it('is reported once, as a total', () => {
    expect(
      realtimePublicServerEventSchema.safeParse({
        schemaVersion: 1,
        requestId,
        sequence: 9,
        type: 'session.closed',
        reason: 'client_closed',
        units: [
          { unit: 'input_transcription_milliseconds', quantity: 500 },
          { unit: 'input_transcription_milliseconds', quantity: 700 },
        ],
        usageSource: 'provider_reported',
        closedAt: '2026-10-15T10:00:00.000Z',
      }).success,
    ).toBe(false);
  });
});

describe('the input transcript of a session', () => {
  it('arrives as transcript events whose source is the caller’s audio', () => {
    for (const event of [
      { type: 'transcript.delta', text: 'Hola' },
      { type: 'transcript.done', transcript: 'Hola, ¿qué tal?' },
    ]) {
      expect(
        realtimePublicServerEventSchema.safeParse({
          schemaVersion: 1,
          requestId,
          sequence: 4,
          source: 'input_audio',
          itemId: 'item_user_1',
          contentIndex: 0,
          ...event,
        }).success,
      ).toBe(true);
    }
  });
});

describe('realtime capabilities in the catalogue', () => {
  it('declares input transcription and the realtime encodings', () => {
    const parsed = modelRealtimeCapabilitiesSchema.parse(realtime);
    expect(parsed.inputAudioTranscription).toBe(true);
    expect(parsed.audioFormats?.input).toEqual(['pcm16_24khz', 'g711_ulaw', 'g711_alaw']);
  });

  it('still reads a catalogue written before 4.1.0, which declares neither', () => {
    const older = modelRealtimeCapabilitiesSchema.parse({
      transports: ['websocket'],
      sessionKinds: ['conversation'],
    });
    expect(older.inputAudioTranscription).toBeUndefined();
    expect(older.audioFormats).toBeUndefined();
  });

  it('refuses an encoding declared twice, a model that takes no input encoding, and file media types', () => {
    const base = { ...realtime, audioFormats: { ...realtime.audioFormats } };
    expect(
      modelCapabilitiesSchema.safeParse({
        ...voiceModel,
        realtime: { ...base, audioFormats: { input: ['pcm16_24khz', 'pcm16_24khz'], output: [] } },
      }).success,
    ).toBe(false);
    expect(
      modelRealtimeCapabilitiesSchema.safeParse({
        ...base,
        audioFormats: { input: [], output: ['pcm16_24khz'] },
      }).success,
    ).toBe(false);
    expect(
      modelRealtimeCapabilitiesSchema.safeParse({
        ...base,
        audioFormats: { input: ['audio/pcm'], output: [] },
      }).success,
    ).toBe(false);
    // Positive control: the same shape with one of each parses.
    expect(
      modelRealtimeCapabilitiesSchema.safeParse({
        ...base,
        audioFormats: { input: ['pcm16_24khz'], output: [] },
      }).success,
    ).toBe(true);
  });

  it('is strict: an undeclared capability is refused, not stripped', () => {
    expect(
      modelRealtimeCapabilitiesSchema.safeParse({ ...realtime, inputTranscriptionModel: 'x' })
        .success,
    ).toBe(false);
  });

  it('exports the realtime encodings from the audio vocabulary, unchanged', () => {
    expect(realtimeAudioFormatSchema.options).toEqual(['pcm16_24khz', 'g711_ulaw', 'g711_alaw']);
  });
});

describe('checkRealtimeSessionOpen', () => {
  const model = { capabilities: voiceModel, audio: voiceAudio };

  it('accepts an open the model can serve', () => {
    expect(checkRealtimeSessionOpen(open(), model)).toEqual([]);
  });

  it('refuses input transcription on a model that does not declare it', () => {
    for (const flag of [false, undefined]) {
      const capabilities = modelCapabilitiesSchema.parse({
        ...voiceModel,
        realtime: { ...realtime, inputAudioTranscription: flag },
      });
      expect(checkRealtimeSessionOpen(open(), { capabilities, audio: voiceAudio })).toEqual([
        {
          param: 'config.inputAudioTranscription',
          message: 'this model does not transcribe its input inside a conversation',
        },
      ]);
      // Control: the same model without transcription is accepted.
      expect(
        checkRealtimeSessionOpen(open({ inputAudioTranscription: undefined }), {
          capabilities,
          audio: voiceAudio,
        }),
      ).toEqual([]);
    }
  });

  it('refuses a voice and encodings the model does not publish, never a language', () => {
    const issues = checkRealtimeSessionOpen(
      open({
        voice: 'ara',
        inputAudioFormat: 'g711_ulaw',
        inputAudioTranscription: { language: 'es' },
      }),
      {
        capabilities: modelCapabilitiesSchema.parse({
          ...voiceModel,
          realtime: { ...realtime, audioFormats: { input: ['pcm16_24khz'], output: [] } },
        }),
        audio: voiceAudio,
      },
    );
    expect(issues.map((issue) => issue.param)).toEqual([
      'config.inputAudioFormat',
      'config.outputAudioFormat',
      'config.voice',
    ]);
  });

  it('refuses a session kind or transport the model does not hold', () => {
    const transcription = realtimeSessionOpenSchema.parse({
      type: 'session.open',
      kind: 'transcription',
      config: {
        inputAudioFormat: 'pcm16_24khz',
        turnDetection: { type: 'none' },
        inputAudioTranscription: {},
      },
    });
    expect(checkRealtimeSessionOpen(transcription, model).map((issue) => issue.param)).toEqual([
      'kind',
    ]);
    const { realtime: _, ...textOnly } = voiceModel;
    expect(checkRealtimeSessionOpen(open(), { capabilities: textOnly })).toEqual([
      { param: 'kind', message: 'this model holds no realtime session' },
    ]);
  });

  it('does not check what an older catalogue leaves undeclared', () => {
    const capabilities = modelCapabilitiesSchema.parse({
      ...voiceModel,
      realtime: { transports: ['websocket'], sessionKinds: ['conversation'] },
    });
    // Voice and language are not checked without `audio`, encodings without
    // `audioFormats`; transcription is still refused, because asking for it
    // starts a charge the model never declared.
    expect(
      checkRealtimeSessionOpen(open({ voice: 'anyone', inputAudioFormat: 'g711_alaw' }), {
        capabilities,
      }).map((issue) => issue.param),
    ).toEqual(['config.inputAudioTranscription']);
  });
});
