import { describe, expect, it } from 'bun:test';
import type { RealtimeServerEvent } from '@kaana.ai/contracts';
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
  it("serves Kaana's voice catalogue, with what each model accepts", async () => {
    const { kaana } = createFakeKaana();
    const models = await kaana.models.list();
    expect(models.map((model) => model.modelId)).toEqual(
      voiceCatalogue.map((model) => model.modelId),
    );
    const voice = models.find((model) => model.modelId === VOICE);
    expect(voice?.capabilities.realtime?.parameters?.toolChoice).toEqual(['auto']);
    expect(voice?.audio?.detectsLanguage).toBe(true);
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

  it('refuses before anything is held what the model does not serve, naming the field', async () => {
    const { kaana, server } = createFakeKaana();
    for (const [config, param] of [
      [{ outputModalities: ['text'] }, 'config.outputModalities'],
      [{ inputAudioTranscription: { language: 'gl' } }, 'config.inputAudioTranscription.language'],
      [{ toolChoice: 'required' }, 'config.toolChoice'],
      [{ maxOutputTokens: 100 }, 'config.maxOutputTokens'],
      [{ voice: 'nobody' }, 'config.voice'],
    ] as const) {
      const error = await refusal(kaana.realtime.connect(voiceCall(config as never)));
      expect(error.param).toBe(param);
    }
    expect(server.sessions).toHaveLength(0);
    // A limit no server enforces is not in the contract: the client refuses it.
    const unenforced = voiceCall();
    (unenforced.limits as Record<string, unknown>).maxTextItems = 0;
    await refusal(kaana.realtime.connect(unenforced));
  });

  it('acknowledges an update it cannot serve, then refuses it without ending the session', async () => {
    const { kaana, server } = createFakeKaana();
    const session = await kaana.realtime.connect(voiceCall());
    const events = session[Symbol.asyncIterator]();
    await next(events);
    session.send({ type: 'session.update', config: { toolChoice: 'none' } });
    expect((await next(events)).type).toBe('command.accepted');
    const refused = await next(events);
    expect(refused.type === 'error' && !refused.fatal && refused.error.param).toBe(
      'config.toolChoice',
    );
    session.send({ type: 'session.update', config: { instructions: 'Shorter.' } });
    expect((await next(events)).type).toBe('command.accepted');
    expect(server.sessions[0]?.commands.map((command) => command.type)).toEqual(['session.update']);
    await session.close();
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

  it('transcribes what a model takes, and refuses what it does not before anything is held', async () => {
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

    const named = await refusal(
      kaana.audio.transcriptions.create({
        model: 'openai/whisper-large-v3-turbo',
        file: audio,
        mediaType: 'audio/webm',
        language: 'es',
      }),
    );
    expect(named.param).toBe('language');
    expect(named.message).toContain('omit it to let the model detect the language');

    const prompted = await refusal(
      kaana.audio.transcriptions.create({
        model: 'x-ai/grok-voice-transcribe-2.0',
        file: audio,
        mediaType: 'audio/webm',
        prompt: 'Kaana',
      }),
    );
    expect(prompted.param).toBe('prompt');
  });
});
