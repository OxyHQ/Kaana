import { describe, expect, it } from 'bun:test';
import {
  checkSpeechRequest,
  type ModelAudioCapabilities,
  modelAudioCapabilitiesSchema,
  modelSchema,
  speechMediaType,
  speechRequestSchema,
  transcriptionJsonRequestSchema,
  transcriptionSchema,
  MAX_TRANSCRIPTION_AUDIO_BASE64_LENGTH,
  MAX_TRANSCRIPTION_FILE_BYTES,
} from '../index.js';
import { audio, requestId, speechModel } from './fixtures.js';

const elevenLabs: ModelAudioCapabilities = modelAudioCapabilitiesSchema.parse(audio);

/** Deepgram refuses `speed` today (`internal/provider/deepgram/adapter.go`): it publishes none. */
const deepgram: ModelAudioCapabilities = modelAudioCapabilitiesSchema.parse({
  voices: [{ id: 'aura-2-thalia-en', name: 'Thalia', languages: ['en'] }],
  languages: ['en'],
  audioFormats: { input: [], output: ['audio/mpeg', 'audio/wav'] },
  maxInputCharacters: 2000,
});

const request = (fields: Record<string, unknown>) =>
  speechRequestSchema.parse({
    model: 'elevenlabs/eleven-multilingual',
    input: 'Hola',
    voice: 'rachel',
    ...fields,
  });

describe('audio capabilities in the catalogue', () => {
  it('publishes voices, languages, formats, speed and the character limit per model', () => {
    const model = modelSchema.parse(speechModel);
    expect(model.audio?.voices.map((voice) => voice.id)).toEqual(['rachel', 'adam']);
    expect(model.audio?.languages).toEqual(['en', 'es-ES']);
    expect(model.audio?.audioFormats.output).toEqual(['audio/mpeg', 'audio/pcm']);
    expect(model.audio?.speed).toEqual({ min: 0.7, max: 1.2 });
    expect(model.audio?.maxInputCharacters).toBe(5000);
  });

  it('leaves speed out for a model that accepts no speed control', () => {
    expect(deepgram.speed).toBeUndefined();
  });

  it('refuses a voice declared twice', () => {
    expect(
      modelAudioCapabilitiesSchema.safeParse({
        ...audio,
        voices: [audio.voices[0], audio.voices[0]],
      }).success,
    ).toBe(false);
  });

  it('refuses a voice speaking a language the model does not declare', () => {
    expect(
      modelAudioCapabilitiesSchema.safeParse({
        ...audio,
        voices: [{ id: 'rachel', name: 'Rachel', languages: ['fr'] }],
      }).success,
    ).toBe(false);
  });

  it('refuses a speed range that ends below where it starts, or leaves the contract bound', () => {
    expect(
      modelAudioCapabilitiesSchema.safeParse({ ...audio, speed: { min: 1.2, max: 0.7 } }).success,
    ).toBe(false);
    expect(
      modelAudioCapabilitiesSchema.safeParse({ ...audio, speed: { min: 0.1, max: 1 } }).success,
    ).toBe(false);
  });

  it('refuses a language that is not a BCP 47 tag', () => {
    expect(
      modelAudioCapabilitiesSchema.safeParse({ ...audio, languages: ['english'] }).success,
    ).toBe(false);
  });
});

describe('checking a speech request before a grant is asked for', () => {
  it('accepts what the model publishes', () => {
    expect(checkSpeechRequest(request({ speed: 1.2 }), elevenLabs)).toEqual([]);
    expect(checkSpeechRequest(request({ voice: 'aura-2-thalia-en' }), deepgram)).toEqual([]);
  });

  it('refuses speed 1.5 to an ElevenLabs model that publishes 0.7–1.2, naming the param', () => {
    expect(checkSpeechRequest(request({ speed: 1.5 }), elevenLabs)).toEqual([
      { param: 'speed', message: 'speed must be between 0.7 and 1.2 for this model' },
    ]);
  });

  it('refuses any speed to a model that publishes no speed control', () => {
    const issues = checkSpeechRequest(request({ voice: 'aura-2-thalia-en', speed: 1 }), deepgram);
    expect(issues.map((issue) => issue.param)).toEqual(['speed']);
  });

  it('refuses a voice the model does not have', () => {
    expect(checkSpeechRequest(request({ voice: 'nobody' }), elevenLabs)[0]?.param).toBe('voice');
  });

  it('refuses a format the model does not produce', () => {
    expect(checkSpeechRequest(request({ response_format: 'flac' }), elevenLabs)[0]?.param).toBe(
      'response_format',
    );
    // Absent means mp3, which both publish.
    expect(checkSpeechRequest(request({}), elevenLabs)).toEqual([]);
  });

  it('counts characters in UTF-16 code units, as the adapters do', () => {
    const limited = { ...elevenLabs, maxInputCharacters: 3 };
    // One emoji is two UTF-16 units: two of them are four, over a limit of three.
    expect(checkSpeechRequest(request({ input: '😀😀' }), limited)[0]?.param).toBe('input');
    expect(checkSpeechRequest(request({ input: 'abc' }), limited)).toEqual([]);
  });

  it('reports every issue, in field order', () => {
    const issues = checkSpeechRequest(
      request({ input: 'x'.repeat(5001), voice: 'nobody', response_format: 'wav', speed: 2 }),
      elevenLabs,
    );
    expect(issues.map((issue) => issue.param)).toEqual([
      'input',
      'voice',
      'response_format',
      'speed',
    ]);
  });
});

describe('speech and speechStream requests', () => {
  it('streams as raw audio by default and as SSE when asked', () => {
    expect(request({ stream: true }).stream_format).toBeUndefined();
    expect(request({ stream: true, stream_format: 'sse' }).stream_format).toBe('sse');
  });

  it('refuses stream_format without stream', () => {
    expect(
      speechRequestSchema.safeParse({
        model: 'elevenlabs/eleven-multilingual',
        input: 'Hola',
        voice: 'rachel',
        stream_format: 'sse',
      }).success,
    ).toBe(false);
  });

  it('streams only the formats that play as they arrive: mp3 and pcm', () => {
    expect(() => request({ stream: true, response_format: 'pcm' })).not.toThrow();
    for (const format of ['wav', 'flac', 'aac', 'opus']) {
      expect(() => request({ stream: true, response_format: format })).toThrow();
      expect(() => request({ response_format: format })).not.toThrow();
    }
  });

  it('names a concrete model, never a power level', () => {
    expect(() => request({ model: 'instant' })).toThrow();
  });

  it('maps every format to the media type the client receives', () => {
    expect(speechMediaType('mp3')).toBe('audio/mpeg');
    expect(speechMediaType('pcm')).toBe('audio/pcm');
    expect(speechMediaType('opus')).toBe('audio/ogg');
    expect(speechMediaType('wav')).toBe('audio/wav');
  });
});

describe('transcription', () => {
  it('refuses a span that ends before it starts', () => {
    expect(
      transcriptionSchema.safeParse({
        schemaVersion: 1,
        requestId,
        model: 'openai/whisper-1@2023-03-01',
        text: 'Hola',
        words: [{ startMs: 500, endMs: 100, word: 'Hola' }],
        usage: [{ unit: 'audio_input_milliseconds', quantity: 500 }],
      }).success,
    ).toBe(false);
  });
});

describe('transcription JSON upload', () => {
  const upload = (audio: Record<string, unknown>) =>
    transcriptionJsonRequestSchema.safeParse({ model: 'openai/whisper-1', audio }).success;

  it('carries padded base64 of a declared input media type', () => {
    expect(upload({ mediaType: 'audio/wav', data: 'UklGRg==' })).toBe(true);
    expect(upload({ mediaType: 'audio/wav', data: 'UklGRg' })).toBe(false);
    expect(upload({ mediaType: 'video/mp4', data: 'UklGRg==' })).toBe(false);
  });

  it('is bounded at exactly the base64 of the largest multipart file', () => {
    expect(MAX_TRANSCRIPTION_AUDIO_BASE64_LENGTH).toBe(
      Math.ceil(MAX_TRANSCRIPTION_FILE_BYTES / 3) * 4,
    );
  });

  it('refuses a field the multipart form does not have', () => {
    expect(
      transcriptionJsonRequestSchema.safeParse({
        model: 'openai/whisper-1',
        audio: { mediaType: 'audio/wav', data: 'UklGRg==' },
        response_format: 'srt',
      }).success,
    ).toBe(false);
  });
});
