/**
 * Contract set 8.16.0 (Kaana ADR 0012): the `live` session kind, its
 * delegation, and the catalogue's default line of a kind.
 */
import { describe, expect, it } from 'bun:test';
import {
  checkRealtimeSessionOpen,
  INFERENCE_CONTRACT_VERSION,
  MAX_REALTIME_DELEGATION_TEXT_CHARS,
  MAX_REALTIME_VERBATIM_SPAN_CHARS,
  MAX_REALTIME_VERBATIM_SPANS,
  modelCapabilitiesSchema,
  realtimeClientCommandSchema,
  realtimePublicServerEventSchema,
  realtimeSessionOpenSchema,
  realtimeSessionTakes,
} from '../index.js';
import { realtimeServerEventSchema } from '../internal.js';
import { requestId } from './fixtures.js';

const liveConfig = {
  instructions: 'You are Alia: warm, brief, in Spanish.',
  voice: 'marin',
  inputAudioFormat: 'pcm16_24khz',
  outputAudioFormat: 'pcm16_24khz',
  turnDetection: {
    type: 'semantic_vad',
    eagerness: 'auto',
    createResponse: true,
    interruptResponse: true,
  },
  inputAudioTranscription: { language: 'es' },
  delegation: { acknowledgement: 'auto' },
};

const open = (kind: string, config: Record<string, unknown>) =>
  realtimeSessionOpenSchema.safeParse({ type: 'session.open', kind, config });

const live = (changes: Record<string, unknown>) => open('live', { ...liveConfig, ...changes });

const pathsOf = (result: ReturnType<typeof open>) =>
  result.error?.issues.map((issue) => issue.path.join('.')) ?? [];

const command = (fields: Record<string, unknown>) =>
  realtimeClientCommandSchema.safeParse({
    schemaVersion: 1,
    requestId,
    commandId: 'cmd_1',
    ...fields,
  });

const reply = (fields: Record<string, unknown>) =>
  command({ type: 'delegation.reply', delegationId: 'del_1', final: false, ...fields });

const event = <Fields extends Record<string, unknown>>(fields: Fields) => ({
  schemaVersion: 1,
  requestId,
  sequence: 7,
  ...fields,
});

describe('contract set 8.16.0', () => {
  it('is in the set this package declares (8.16.0 or later)', () => {
    const [major = 0, minor = 0] = INFERENCE_CONTRACT_VERSION.split('.').map(Number);
    expect(major > 8 || (major === 8 && minor >= 16)).toBe(true);
  });
});

describe('a live session', () => {
  it('opens with a voice, one format both ways, detected turns and a delegation', () => {
    expect(live({}).success).toBe(true);
    expect(
      live({ delegation: { acknowledgement: 'none' }, inputAudioTranscription: undefined }).success,
    ).toBe(true);
  });

  it('refuses each field the voice needs when it is missing', () => {
    expect(pathsOf(live({ voice: undefined }))).toEqual(['config.voice']);
    expect(pathsOf(live({ outputAudioFormat: undefined }))).toEqual(['config.outputAudioFormat']);
    expect(pathsOf(live({ outputAudioFormat: 'g711_ulaw' }))).toEqual(['config.outputAudioFormat']);
    expect(pathsOf(live({ turnDetection: { type: 'none' } }))).toEqual(['config.turnDetection']);
    expect(pathsOf(live({ delegation: undefined }))).toEqual(['config.delegation']);
  });

  it("refuses what is its caller's to decide", () => {
    for (const [field, value] of [
      ['outputModalities', ['audio']],
      ['maxOutputTokens', 100],
      ['translation', { targetLanguage: 'en' }],
    ] as const) {
      expect(pathsOf(live({ [field]: value }))).toEqual([`config.${field}`]);
    }
    const tool = { type: 'function', name: 'lookup', parameters: { type: 'object' } };
    expect(pathsOf(live({ tools: [tool] }))).toEqual(['config.tools']);
    expect(pathsOf(live({ tools: [tool], toolChoice: 'auto' }))).toEqual([
      'config.tools',
      'config.toolChoice',
    ]);
  });

  it('transcribes with a language only', () => {
    expect(pathsOf(live({ inputAudioTranscription: { prompt: 'names' } }))).toEqual([
      'config.inputAudioTranscription.prompt',
    ]);
    expect(pathsOf(live({ inputAudioTranscription: { interimResults: true } }))).toEqual([
      'config.inputAudioTranscription.interimResults',
    ]);
  });

  it('names a known acknowledgement', () => {
    expect(live({ delegation: { acknowledgement: 'sometimes' } }).success).toBe(false);
    expect(live({ delegation: { acknowledgement: 'auto', delayMs: 400 } }).success).toBe(false);
  });

  it('is the only kind with a delegation', () => {
    const conversation = {
      outputModalities: ['text'],
      inputAudioFormat: 'pcm16_24khz',
      turnDetection: { type: 'server_vad', createResponse: true, interruptResponse: true },
    };
    expect(open('conversation', conversation).success).toBe(true);
    expect(
      pathsOf(open('conversation', { ...conversation, delegation: { acknowledgement: 'auto' } })),
    ).toEqual(['config.delegation']);
  });

  it('is refused by a model whose catalogue holds no live session', () => {
    const parsed = live({});
    if (!parsed.success) throw parsed.error;
    const capabilities = (sessionKinds: string[]) =>
      modelCapabilitiesSchema.parse({
        inputModalities: ['text', 'audio'],
        outputModalities: ['text', 'audio'],
        tools: false,
        parallelToolCalls: false,
        structuredOutput: false,
        jsonMode: false,
        reasoning: false,
        streaming: true,
        promptCaching: false,
        maxContextTokens: 32_000,
        maxOutputTokens: 4096,
        realtime: { transports: ['websocket'], sessionKinds },
      });
    expect(
      checkRealtimeSessionOpen(parsed.data, { capabilities: capabilities(['conversation']) }),
    ).toEqual([{ param: 'kind', message: 'this model holds no live session' }]);
    expect(checkRealtimeSessionOpen(parsed.data, { capabilities: capabilities(['live']) })).toEqual(
      [],
    );
  });
});

describe('delegation commands', () => {
  it('reply with text, verbatim spans of it and whether it is the last part', () => {
    expect(
      reply({
        text: 'Your table is at 19:30, Calle Mayor 12.',
        verbatim: ['19:30', 'Calle Mayor 12'],
        final: true,
      }).success,
    ).toBe(true);
    expect(reply({ text: 'One moment.' }).success).toBe(true);
    const { final: _final, ...withoutFinal } = { text: 'x', final: true };
    expect(
      command({ type: 'delegation.reply', delegationId: 'del_1', ...withoutFinal }).success,
    ).toBe(false);
  });

  it('mark only spans that are part of the text, exactly as written', () => {
    const refused = reply({ text: 'Your code is 4471.', verbatim: ['4471', '4417'] });
    expect(refused.success).toBe(false);
    expect(refused.error?.issues.map((issue) => issue.path)).toEqual([['verbatim', 1]]);
    expect(reply({ text: 'Your code is 4471.', verbatim: [''] }).success).toBe(false);
    const span = 'a'.repeat(MAX_REALTIME_VERBATIM_SPAN_CHARS);
    expect(reply({ text: span, verbatim: [span] }).success).toBe(true);
    expect(reply({ text: `${span}a`, verbatim: [`${span}a`] }).success).toBe(false);
    const spans = Array.from({ length: MAX_REALTIME_VERBATIM_SPANS }, () => 'a');
    expect(reply({ text: 'a', verbatim: spans }).success).toBe(true);
    expect(reply({ text: 'a', verbatim: [...spans, 'a'] }).success).toBe(false);
  });

  it('bound their text in UTF-16 units', () => {
    const longest = 'a'.repeat(MAX_REALTIME_DELEGATION_TEXT_CHARS);
    expect(reply({ text: longest }).success).toBe(true);
    expect(reply({ text: `${longest}a` }).success).toBe(false);
    expect(reply({ text: '' }).success).toBe(false);
    const context = (text: string) =>
      command({ type: 'delegation.context', delegationId: 'del_1', text });
    expect(context('still checking your calendar').success).toBe(true);
    expect(context(longest).success).toBe(true);
    expect(context(`${longest}a`).success).toBe(false);
    expect(context('').success).toBe(false);
  });

  it('name a delegation', () => {
    expect(command({ type: 'delegation.cancel', delegationId: 'del_1' }).success).toBe(true);
    expect(command({ type: 'delegation.cancel', delegationId: '' }).success).toBe(false);
    expect(command({ type: 'delegation.cancel', delegationId: 'd'.repeat(129) }).success).toBe(
      false,
    );
    expect(command({ type: 'delegation.cancel' }).success).toBe(false);
  });
});

describe('delegation events', () => {
  const requested = event({
    type: 'delegation.requested',
    delegationId: 'del_1',
    text: 'book a table',
  });
  const superseded = event({ type: 'delegation.superseded', delegationId: 'del_1' });

  it('are events of the public and the signed session', () => {
    for (const schema of [realtimePublicServerEventSchema, realtimeServerEventSchema]) {
      expect(schema.safeParse(requested).success).toBe(true);
      expect(schema.safeParse(superseded).success).toBe(true);
    }
  });

  it('name their delegation, and a request carries the words', () => {
    expect(
      realtimePublicServerEventSchema.safeParse({ ...requested, delegationId: '' }).success,
    ).toBe(false);
    const { text: _text, ...withoutText } = requested;
    expect(realtimePublicServerEventSchema.safeParse(withoutText).success).toBe(false);
  });
});

describe('capabilities.realtime.defaultKinds', () => {
  const capabilities = (realtime: Record<string, unknown>) =>
    modelCapabilitiesSchema.safeParse({
      inputModalities: ['text', 'audio'],
      outputModalities: ['text', 'audio'],
      tools: false,
      parallelToolCalls: false,
      structuredOutput: false,
      jsonMode: false,
      reasoning: false,
      streaming: true,
      promptCaching: false,
      maxContextTokens: 32_000,
      maxOutputTokens: 4096,
      realtime: { transports: ['websocket'], sessionKinds: ['live', 'conversation'], ...realtime },
    });

  it('names kinds the line holds, each once', () => {
    expect(capabilities({}).success).toBe(true);
    expect(capabilities({ defaultKinds: ['live'] }).success).toBe(true);
    expect(capabilities({ defaultKinds: ['live', 'conversation'] }).success).toBe(true);
    expect(capabilities({ defaultKinds: ['transcription'] }).success).toBe(false);
    expect(capabilities({ defaultKinds: ['live', 'live'] }).success).toBe(false);
    expect(capabilities({ defaultKinds: [] }).success).toBe(false);
  });
});

describe('realtimeSessionTakes', () => {
  it('refuses on a live session what its voice owns, and the delegation commands elsewhere', () => {
    for (const command of ['session.update', 'input_audio.commit', 'input_audio.clear'] as const) {
      expect(realtimeSessionTakes('live', command)).toBe(false);
      expect(realtimeSessionTakes('conversation', command)).toBe(true);
    }
    for (const command of [
      'delegation.reply',
      'delegation.context',
      'delegation.cancel',
    ] as const) {
      expect(realtimeSessionTakes('live', command)).toBe(true);
      for (const kind of ['conversation', 'transcription', 'translation'] as const) {
        expect(realtimeSessionTakes(kind, command)).toBe(false);
      }
    }
    for (const command of ['input_audio.append', 'session.close'] as const) {
      expect(realtimeSessionTakes('live', command)).toBe(true);
    }
  });
});
