/**
 * Contract set 8.1.0 (Kaana ADR 0006): a transcription session's interim text.
 */
import { describe, expect, it } from 'bun:test';
import {
  INFERENCE_CONTRACT_VERSION,
  realtimePublicServerEventSchema,
  realtimeSessionOpenSchema,
} from '../index.js';
import { realtimeServerEventSchema } from '../internal.js';
import { requestId } from './fixtures.js';

const transcription = (inputAudioTranscription: Record<string, unknown>) =>
  realtimeSessionOpenSchema.safeParse({
    type: 'session.open',
    kind: 'transcription',
    config: {
      inputAudioFormat: 'pcm16_24khz',
      turnDetection: { type: 'server_vad', createResponse: false, interruptResponse: false },
      inputAudioTranscription,
    },
  });

const conversation = (inputAudioTranscription: Record<string, unknown>) =>
  realtimeSessionOpenSchema.safeParse({
    type: 'session.open',
    kind: 'conversation',
    config: {
      outputModalities: ['text'],
      inputAudioFormat: 'pcm16_24khz',
      turnDetection: { type: 'server_vad', createResponse: true, interruptResponse: true },
      inputAudioTranscription,
    },
  });

const interim = (fields: Record<string, unknown>) => ({
  schemaVersion: 1,
  requestId,
  sequence: 4,
  type: 'transcript.interim',
  source: 'input_audio',
  itemId: 'item_1',
  contentIndex: 0,
  text: 'hello wor',
  ...fields,
});

describe('contract set 8.1.0', () => {
  it('is in the set this package declares (8.1.0 or later)', () => {
    const [major = 0, minor = 0] = INFERENCE_CONTRACT_VERSION.split('.').map(Number);
    expect(major > 8 || (major === 8 && minor >= 1)).toBe(true);
  });
});

describe('interimResults', () => {
  it('is asked for by a transcription session', () => {
    expect(transcription({ language: 'en', interimResults: true }).success).toBe(true);
    expect(transcription({ interimResults: false }).success).toBe(true);
  });

  it('is refused in a conversation, which never sends interim text', () => {
    const refused = conversation({ interimResults: true });
    expect(refused.success).toBe(false);
    expect(refused.error?.issues[0]?.path).toEqual([
      'config',
      'inputAudioTranscription',
      'interimResults',
    ]);
    expect(conversation({ interimResults: false }).success).toBe(true);
    expect(conversation({}).success).toBe(true);
  });

  it('is a boolean', () => {
    expect(transcription({ interimResults: 'yes' }).success).toBe(false);
  });
});

describe('transcript.interim', () => {
  it('is an event of the public and the signed session', () => {
    expect(realtimePublicServerEventSchema.safeParse(interim({})).success).toBe(true);
    expect(realtimeServerEventSchema.safeParse(interim({})).success).toBe(true);
  });

  it("describes only the caller's audio", () => {
    expect(
      realtimePublicServerEventSchema.safeParse(interim({ source: 'output_audio' })).success,
    ).toBe(false);
  });

  it('names its item and carries text, even none', () => {
    expect(realtimePublicServerEventSchema.safeParse(interim({ text: '' })).success).toBe(true);
    expect(realtimePublicServerEventSchema.safeParse(interim({ itemId: '' })).success).toBe(false);
    const { text: _text, ...withoutText } = interim({});
    expect(realtimePublicServerEventSchema.safeParse(withoutText).success).toBe(false);
  });
});
