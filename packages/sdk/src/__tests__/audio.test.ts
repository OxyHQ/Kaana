import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { MAX_TRANSCRIPTION_FILE_BYTES, speechRequestSchema } from '@kaana.ai/contracts';
import {
  decodeBase64,
  encodeBase64,
  Kaana,
  KaanaInvalidRequestError,
  KaanaProtocolError,
  KaanaUnavailableError,
} from '../index.js';
import {
  CREDENTIAL,
  FakeKaana,
  nextRequestId,
  publicError,
  SPEECH_MODEL,
  sse,
} from './fakeKaana.js';

let fake: FakeKaana;
let kaana: Kaana;

beforeAll(() => {
  fake = new FakeKaana().start();
});
afterAll(() => fake.stop());
beforeEach(() => {
  fake.handlers.clear();
  fake.requests.length = 0;
  kaana = new Kaana({ credential: CREDENTIAL, baseURL: fake.baseURL });
});

const speechRequest = { model: 'elevenlabs/eleven-multilingual', input: 'Hola', voice: 'rachel' };

describe('audio.speech', () => {
  it('returns the bytes with the served model and usage from the headers', async () => {
    const result = await kaana.audio.speech(speechRequest);
    expect(result.audio).toEqual(new Uint8Array([0x49, 0x44, 0x33, 4, 0, 0]));
    expect(result.mediaType).toBe('audio/mpeg');
    expect(result.model).toBe(SPEECH_MODEL);
    expect(result.usage).toEqual([
      { unit: 'characters', quantity: 4 },
      { unit: 'requests', quantity: 1 },
    ]);
    expect(result.requestId).toStartWith('req_fake_');
    expect(fake.requests[0].headers.get('Accept')).toBe('audio/*');
  });

  it('refuses audio of another media type and a malformed usage header', async () => {
    fake.on(
      'POST /audio/speech',
      () =>
        new Response(new Uint8Array([1]), {
          headers: {
            'Kaana-Request-Id': nextRequestId(),
            'Content-Type': 'audio/wav',
            'Kaana-Model': SPEECH_MODEL,
            'Kaana-Usage': 'characters=4',
          },
        }),
    );
    await expect(kaana.audio.speech(speechRequest)).rejects.toBeInstanceOf(KaanaProtocolError);
    fake.on(
      'POST /audio/speech',
      () =>
        new Response(new Uint8Array([1]), {
          headers: {
            'Kaana-Request-Id': nextRequestId(),
            'Content-Type': 'audio/mpeg',
            'Kaana-Model': SPEECH_MODEL,
            'Kaana-Usage': 'characters=four',
          },
        }),
    );
    await expect(kaana.audio.speech(speechRequest)).rejects.toBeInstanceOf(KaanaProtocolError);
  });

  it('accepts pcm with a rate parameter', async () => {
    fake.on(
      'POST /audio/speech',
      () =>
        new Response(new Uint8Array([1, 2]), {
          headers: {
            'Kaana-Request-Id': nextRequestId(),
            'Content-Type': 'audio/pcm;rate=24000',
            'Kaana-Model': SPEECH_MODEL,
            'Kaana-Usage': 'characters=4',
          },
        }),
    );
    const result = await kaana.audio.speech({ ...speechRequest, response_format: 'pcm' });
    expect(result.mediaType).toBe('audio/pcm');
  });
});

describe('audio.speechStream', () => {
  const frames = (requestId: string, failAfter: boolean) => [
    {
      event: 'speech.audio.delta',
      data: { type: 'speech.audio.delta', audio: encodeBase64(new Uint8Array([1, 2, 3])) },
    },
    {
      event: 'speech.audio.delta',
      data: { type: 'speech.audio.delta', audio: encodeBase64(new Uint8Array([4])) },
    },
    failAfter
      ? {
          event: 'error',
          data: { type: 'error', error: publicError('provider_error', { requestId }) },
        }
      : {
          event: 'speech.audio.done',
          data: {
            type: 'speech.audio.done',
            requestId,
            model: SPEECH_MODEL,
            usage: [{ unit: 'characters', quantity: 4 }],
          },
        },
  ];

  it('sends stream:true with stream_format sse and yields audio in order', async () => {
    fake.on('POST /audio/speech', (_request, recorded) => {
      const request = speechRequestSchema.parse(recorded.json);
      expect(request.stream).toBe(true);
      expect(request.stream_format).toBe('sse');
      const requestId = nextRequestId();
      return sse(requestId, frames(requestId, false));
    });
    const stream = kaana.audio.speechStream(speechRequest);
    const chunks: Uint8Array[] = [];
    for await (const chunk of stream) chunks.push(chunk);
    expect(chunks).toEqual([new Uint8Array([1, 2, 3]), new Uint8Array([4])]);
    expect(await stream.mediaType).toBe('audio/mpeg');
    const done = await stream.done;
    expect(done.model).toBe(SPEECH_MODEL);
    expect(done.usage).toEqual([{ unit: 'characters', quantity: 4 }]);
    expect(done.requestId).toBe(await stream.requestId);
  });

  it('exposes a ReadableStream and delivers a mid-stream failure typed', async () => {
    fake.on('POST /audio/speech', () => {
      const requestId = nextRequestId();
      return sse(requestId, frames(requestId, true));
    });
    const stream = kaana.audio.speechStream(speechRequest);
    const reader = stream.toReadableStream().getReader();
    expect((await reader.read()).value).toEqual(new Uint8Array([1, 2, 3]));
    expect((await reader.read()).value).toEqual(new Uint8Array([4]));
    const error = await reader.read().catch((e) => e);
    expect(error).toBeInstanceOf(KaanaUnavailableError);
    expect(error.afterOutput).toBe(true);
    await expect(stream.done).rejects.toBe(error);
  });

  it('refuses a format that cannot stream before sending anything', () => {
    expect(() => kaana.audio.speechStream({ ...speechRequest, response_format: 'wav' })).toThrow(
      KaanaInvalidRequestError,
    );
    expect(fake.requests.length).toBe(0);
  });
});

describe('audio.transcriptions', () => {
  it('uploads bytes as JSON with inline base64', async () => {
    const audio = new Uint8Array([82, 73, 70, 70, 1, 2, 3]);
    const transcription = await kaana.audio.transcriptions.create({
      model: 'openai/whisper-1',
      file: audio,
      mediaType: 'audio/wav',
      language: 'es-ES',
    });
    expect(transcription.text).toBe('heard 7 bytes');
    const sent = fake.requests[0].json as { audio: { data: string; mediaType: string } };
    expect(decodeBase64(sent.audio.data)).toEqual(audio);
    expect(sent.audio.mediaType).toBe('audio/wav');
  });

  it('uploads a Blob as multipart', async () => {
    const transcription = await kaana.audio.transcriptions.create({
      model: 'openai/whisper-1',
      file: new Blob([new Uint8Array(10)]),
      mediaType: 'audio/mpeg',
      prompt: 'Kaana, Alia',
    });
    expect(transcription.text).toBe('heard 10 bytes');
    const form = fake.requests[0].form;
    expect(form?.get('model')).toBe('openai/whisper-1');
    expect(form?.get('prompt')).toBe('Kaana, Alia');
    const file = form?.get('file') as File;
    expect(file.type).toBe('audio/mpeg');
    expect(file.name).toBe('audio.mp3');
  });

  it('refuses an oversized file before sending it', async () => {
    await expect(
      kaana.audio.transcriptions.create({
        model: 'openai/whisper-1',
        file: new Uint8Array(MAX_TRANSCRIPTION_FILE_BYTES + 1),
        mediaType: 'audio/wav',
      }),
    ).rejects.toBeInstanceOf(KaanaInvalidRequestError);
    expect(fake.requests.length).toBe(0);
  });
});

describe('base64', () => {
  it('round-trips every length', () => {
    for (let length = 0; length < 64; length += 1) {
      const bytes = Uint8Array.from({ length }, (_, index) => (index * 37 + 11) & 0xff);
      expect(decodeBase64(encodeBase64(bytes))).toEqual(bytes);
      expect(encodeBase64(bytes)).toBe(Buffer.from(bytes).toString('base64'));
    }
  });

  it('refuses a character outside the alphabet', () => {
    expect(() => decodeBase64('ab*d')).toThrow();
  });
});
