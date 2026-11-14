import { describe, expect, it } from 'bun:test';
import { INFERENCE_CONTRACT_VERSION, type RealtimeServerEvent } from '@kaana.ai/contracts';
import {
  KaanaConnectionError,
  KaanaInvalidRequestError,
  type RealtimeConnectRequest,
} from '../index.js';
import { createFakeKaana, voiceCatalogue } from '../testing/index.js';

const VOICE = 'x-ai/grok-voice-3.0';

/** The session a voice client opens on xAI: what Alia sends. */
function voiceCall(config: Partial<RealtimeConnectRequest['config']> = {}): RealtimeConnectRequest {
  return {
    model: VOICE,
    type: 'session.open',
    kind: 'conversation',
    transport: 'websocket',
    config: {
      instructions: 'Answer briefly.',
      outputModalities: ['audio'],
      voice: 'eve',
      inputAudioFormat: 'pcm16_24khz',
      outputAudioFormat: 'pcm16_24khz',
      turnDetection: { type: 'server_vad', createResponse: true, interruptResponse: true },
      inputAudioTranscription: { language: 'es-ES' },
      tools: [{ type: 'function', name: 'lookup', parameters: { type: 'object', properties: {} } }],
      toolChoice: 'auto',
      ...config,
    },
    limits: { maxDurationMs: 60_000, maxResponses: 500 },
    clientSessionId: 'call_1',
  };
}

async function next(iterator: AsyncIterator<RealtimeServerEvent>): Promise<RealtimeServerEvent> {
  const result = await iterator.next();
  if (result.done) throw new Error('the session ended');
  return result.value;
}

async function refusal(promise: Promise<unknown>): Promise<KaanaInvalidRequestError> {
  try {
    await promise;
  } catch (error) {
    if (error instanceof KaanaInvalidRequestError) return error;
    throw error;
  }
  throw new Error('the request was not refused');
}

describe('@kaana.ai/sdk/testing', () => {
  it("serves Kaana's voice catalogue, priced per minute and with no provider variant", async () => {
    const { kaana, server } = createFakeKaana();
    const models = await kaana.models.list();
    // The client states the set it parses, which Kaana renders the catalogue in.
    expect(server.requests[0]?.headers.get('Kaana-Contract-Version')).toBe(
      INFERENCE_CONTRACT_VERSION,
    );
    expect(models.map((model) => model.modelId)).toEqual(
      voiceCatalogue.map((model) => model.modelId),
    );
    const voice = models.find((model) => model.modelId === VOICE);
    expect(voice?.pricePerMinute?.currency).toBe('USD');
    expect(Number(voice?.pricePerMinute?.amount)).toBeGreaterThan(0);
    expect(JSON.stringify(models)).not.toContain('"parameters"');
  });

  it('opens the session a voice client opens, and settles it on close', async () => {
    const { kaana, server } = createFakeKaana();
    const session = await kaana.realtime.connect(voiceCall());
    const events = session[Symbol.asyncIterator]();
    expect((await next(events)).type).toBe('session.created');
    const [opened] = server.sessions;
    opened?.emit({
      type: 'response.done',
      responseId: 'resp_1',
      status: 'completed',
      units: [],
      usageSource: 'oxy_measured',
    });
    expect((await next(events)).type).toBe('response.done');
    expect(await session.close()).toEqual([]);
    expect(opened?.closed).toBe(true);
  });

  it('opens whatever the contract offers, and refuses only what the model lacks', async () => {
    const { kaana, server } = createFakeKaana();
    for (const config of [
      { outputModalities: ['text'], voice: undefined, outputAudioFormat: undefined },
      { inputAudioTranscription: { language: 'gl', prompt: 'Kaana, Alia' } },
      { toolChoice: 'none' },
      { maxOutputTokens: 100 },
    ] as const) {
      const session = await kaana.realtime.connect(voiceCall(config as never));
      await session.close();
    }
    expect(server.sessions).toHaveLength(4);
    const error = await refusal(kaana.realtime.connect(voiceCall({ voice: 'nobody' })));
    expect(error.param).toBe('config.voice');
    expect(server.sessions).toHaveLength(4);
    // A limit no server enforces is not in the contract: the client refuses it.
    const unenforced = voiceCall();
    (unenforced.limits as Record<string, unknown>).maxTextItems = 0;
    await refusal(kaana.realtime.connect(unenforced));
  });

  it('applies every update the contract offers', async () => {
    const { kaana, server } = createFakeKaana();
    const session = await kaana.realtime.connect(voiceCall());
    const events = session[Symbol.asyncIterator]();
    await next(events);
    session.send({ type: 'session.update', config: { toolChoice: 'none' } });
    expect((await next(events)).type).toBe('command.accepted');
    session.send({ type: 'session.update', config: { instructions: 'Shorter.' } });
    expect((await next(events)).type).toBe('command.accepted');
    expect(server.sessions[0]?.commands.map((command) => command.type)).toEqual([
      'session.update',
      'session.update',
    ]);
    await session.close();
  });

  it("hands a live session's turns over and refuses what its voice owns", async () => {
    const grok = voiceCatalogue.find((model) => model.modelId === VOICE);
    if (grok?.capabilities.realtime === undefined)
      throw new Error('no realtime line to start from');
    const liveLine = {
      ...grok,
      capabilities: {
        ...grok.capabilities,
        realtime: { ...grok.capabilities.realtime, sessionKinds: ['live' as const] },
      },
    };
    const { kaana, server } = createFakeKaana({ models: [liveLine] });
    const session = await kaana.realtime.connect({
      model: VOICE,
      type: 'session.open',
      kind: 'live',
      config: {
        voice: 'eve',
        inputAudioFormat: 'pcm16_24khz',
        outputAudioFormat: 'pcm16_24khz',
        turnDetection: { type: 'server_vad', createResponse: true, interruptResponse: true },
        delegation: { acknowledgement: 'auto' },
      },
    });
    const events = session[Symbol.asyncIterator]();
    await next(events);
    const opened = server.sessions[0];
    const refusedParam = async () => {
      const event = await next(events);
      if (event.type !== 'error') throw new Error(`expected a refusal, got ${event.type}`);
      return event.error.param;
    };

    opened?.emit({ type: 'delegation.requested', delegationId: 'del_1', text: 'book a table' });
    expect(await next(events)).toMatchObject({
      type: 'delegation.requested',
      delegationId: 'del_1',
    });
    session.send({
      type: 'delegation.reply',
      delegationId: 'del_1',
      text: 'One moment.',
      final: false,
    });
    expect((await next(events)).type).toBe('command.accepted');

    // The person moved on before the final reply: del_1 is superseded.
    opened?.emit({ type: 'delegation.requested', delegationId: 'del_2', text: 'no, for tomorrow' });
    expect(await next(events)).toMatchObject({
      type: 'delegation.superseded',
      delegationId: 'del_1',
    });
    expect((await next(events)).type).toBe('delegation.requested');
    session.send({ type: 'delegation.reply', delegationId: 'del_1', text: 'Booked.', final: true });
    expect(await refusedParam()).toBe('delegationId');

    session.send({
      type: 'delegation.reply',
      delegationId: 'del_2',
      text: 'Booked for 19:30 tomorrow.',
      verbatim: ['19:30'],
      final: true,
    });
    expect((await next(events)).type).toBe('command.accepted');
    session.send({ type: 'delegation.context', delegationId: 'del_2', text: 'done' });
    expect(await refusedParam()).toBe('delegationId');

    session.send({ type: 'session.update', config: { instructions: 'Shorter.' } });
    expect(await refusedParam()).toBe('type');
    session.send({ type: 'input_audio.commit' });
    expect(await refusedParam()).toBe('type');
    expect(opened?.commands.map((command) => command.type)).toEqual([
      'delegation.reply',
      'delegation.reply',
    ]);
    await session.close();

    const { kaana: voiceOnly } = createFakeKaana();
    const conversation = await voiceOnly.realtime.connect(voiceCall());
    const conversationEvents = conversation[Symbol.asyncIterator]();
    await next(conversationEvents);
    conversation.send({ type: 'delegation.cancel', delegationId: 'del_1' });
    expect(await next(conversationEvents)).toMatchObject({
      type: 'error',
      error: { param: 'type' },
    });
    await conversation.close();
  });

  it('resumes a dropped connection and replays what the client missed', async () => {
    const { kaana, server } = createFakeKaana();
    const session = await kaana.realtime.connect(voiceCall());
    const events = session[Symbol.asyncIterator]();
    await next(events);
    const opened = server.sessions[0];
    opened?.drop();
    // The drop reaches the reader as the SDK's connection error.
    await expect(next(events)).rejects.toBeInstanceOf(KaanaConnectionError);
    opened?.emit({ type: 'input_audio.cleared' });
    await session.resume();
    const resumed = session[Symbol.asyncIterator]();
    expect((await next(resumed)).type).toBe('input_audio.cleared');
    expect((await next(resumed)).type).toBe('session.resumed');
    await session.close();
  });

  it('transcribes any language with any hint, and refuses an upload the model does not read', async () => {
    const { kaana } = createFakeKaana({
      transcribe: ({ model }) => ({ text: `heard by ${model.modelId}`, durationMs: 2500 }),
    });
    const audio = new Uint8Array([26, 69, 223, 163]);
    const whisper = await kaana.audio.transcriptions.create({
      model: 'openai/whisper-large-v3-turbo',
      file: audio,
      mediaType: 'audio/webm',
      prompt: 'Kaana, Alia',
    });
    expect(whisper.text).toBe('heard by openai/whisper-large-v3-turbo');
    expect(whisper.usage).toEqual([{ unit: 'audio_input_milliseconds', quantity: 2500 }]);

    // Any language and a prompt reach Kaana, which translates them.
    const named = await kaana.audio.transcriptions.create({
      model: 'x-ai/grok-voice-transcribe-2.0',
      file: audio,
      mediaType: 'audio/webm',
      language: 'gl',
      prompt: 'Kaana',
    });
    expect(named.text).toBe('heard by x-ai/grok-voice-transcribe-2.0');

    const unread = await refusal(
      kaana.audio.transcriptions.create({
        model: 'openai/whisper-large-v3-turbo',
        file: audio,
        mediaType: 'audio/aac' as never,
      }),
    );
    expect(unread.param).toBe('file');
  });
});
