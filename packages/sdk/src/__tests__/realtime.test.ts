import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import type { RealtimeServerEvent } from '@kaana.ai/contracts';
import {
  Kaana,
  KaanaAuthenticationError,
  KaanaConnectionError,
  KaanaInvalidRequestError,
  MAX_REALTIME_AUDIO_FRAME_BYTES,
} from '../index.js';
import { CREDENTIAL, FakeKaana, sessionConfig } from './fakeKaana.js';

let fake: FakeKaana;
let kaana: Kaana;

beforeAll(() => {
  fake = new FakeKaana().start();
});
afterAll(() => fake.stop());
beforeEach(() => {
  fake.frames.length = 0;
  fake.dropAfter = undefined;
  kaana = new Kaana({ credential: CREDENTIAL, baseURL: fake.baseURL });
});

const open = {
  model: 'openai/gpt-realtime',
  type: 'session.open',
  kind: 'conversation',
  config: sessionConfig,
} as const;

async function next(
  iterator: AsyncIterator<RealtimeServerEvent>,
  type: RealtimeServerEvent['type'],
): Promise<RealtimeServerEvent> {
  while (true) {
    const result = await iterator.next();
    if (result.done) throw new Error(`the session ended before ${type}`);
    if (result.value.type === type) return result.value;
  }
}

describe('realtime.connect', () => {
  it('opens with the credential subprotocols and the session.open frame', async () => {
    const session = await kaana.realtime.connect(open as never, { onBehalfOfUserId: 'user_1' });
    expect(session.sessionId).toStartWith('req_fake_');
    expect(fake.frames[0]).toEqual({
      type: 'session.open',
      kind: 'conversation',
      config: sessionConfig,
    });
    const fakeSession = fake.sessions.get(session.sessionId);
    expect(fakeSession?.socket?.data.model).toBe('openai/gpt-realtime');
    expect(fakeSession?.socket?.data.user).toBe('user_1');
    const iterator = session[Symbol.asyncIterator]();
    expect((await iterator.next()).value?.type).toBe('session.created');
    expect(await session.close()).toEqual([
      { unit: 'audio_input_milliseconds', quantity: 1000 },
      { unit: 'output_tokens', quantity: 5 },
    ]);
  });

  it('rejects typed when the server refuses before opening', async () => {
    const client = new Kaana({ credential: 'oxy_sk_wrong', baseURL: fake.baseURL });
    await expect(client.realtime.connect(open as never)).rejects.toBeInstanceOf(
      KaanaAuthenticationError,
    );
  });

  it('refuses an invalid open frame before connecting', async () => {
    await expect(
      kaana.realtime.connect({
        ...open,
        config: { ...sessionConfig, outputModalities: undefined },
      } as never),
    ).rejects.toBeInstanceOf(KaanaInvalidRequestError);
    expect(fake.frames.length).toBe(0);
  });
});

describe('a session', () => {
  it('runs a tool call round trip with filled command identities', async () => {
    const session = await kaana.realtime.connect(open as never);
    const events = session[Symbol.asyncIterator]();
    session.send({ type: 'response.create' });
    const call = await next(events, 'tool_call');
    if (call.type !== 'tool_call') throw new Error('unreachable');
    expect(call.name).toBe('lookup');
    session.send({
      type: 'conversation.item.create',
      item: { type: 'function_call_output', callId: call.toolCallId, output: '{"sol":true}' },
    });
    session.send({ type: 'response.create', response: { outputModalities: ['text'] } });
    const text = await next(events, 'text.delta');
    expect(text.type === 'text.delta' && text.text).toBe('Hace sol.');
    const commands = fake.sessions.get(session.sessionId)?.commands as Record<string, unknown>[];
    expect(commands.map((command) => command.type)).toEqual([
      'response.create',
      'conversation.item.create',
      'response.create',
    ]);
    const ids = commands.map((command) => command.commandId);
    expect(new Set(ids).size).toBe(3);
    for (const command of commands) {
      expect(command.requestId).toBe(session.sessionId);
      expect(command.schemaVersion).toBe(1);
    }
    await session.close();
    expect((await events.next()).done).toBe(false); // command.accepted of close
  });

  it('splits audio into bounded append frames', async () => {
    const session = await kaana.realtime.connect(open as never);
    session.appendAudio(new Uint8Array(MAX_REALTIME_AUDIO_FRAME_BYTES * 2 + 10));
    await session.close();
    const commands = (fake.sessions.get(session.sessionId)?.commands ?? []) as { type: string }[];
    const appends = commands.filter((command) => command.type === 'input_audio.append');
    expect(appends.length).toBe(3);
  });

  it('resumes after a dropped connection and replays what it missed once', async () => {
    const session = await kaana.realtime.connect(open as never);
    const events = session[Symbol.asyncIterator]();
    fake.dropAfter = 'input_audio.commit';
    session.send({ type: 'input_audio.commit' });
    const seen: string[] = [];
    const dropped = await (async () => {
      while (true) {
        const result = await events.next();
        if (result.done) return undefined;
        seen.push(result.value.type);
      }
    })().catch((e) => e);
    expect(dropped).toBeInstanceOf(KaanaConnectionError);
    expect(() => session.send({ type: 'input_audio.clear' })).toThrow(KaanaConnectionError);

    await session.resume();
    const resumedFrame = fake.frames.at(-1) as Record<string, unknown>;
    expect(resumedFrame.type).toBe('session.resume');
    expect(resumedFrame.requestId).toBe(session.sessionId);
    // An iterator that threw is finished (generator semantics): iterate anew.
    const resumed = session[Symbol.asyncIterator]();
    const afterResume: string[] = [];
    while (true) {
      const result = await resumed.next();
      if (result.done) break;
      afterResume.push(result.value.type);
      if (result.value.type === 'session.resumed') break;
    }
    // Whatever the drop cut off is replayed, and nothing is delivered twice.
    expect([...seen, ...afterResume]).toEqual([
      'session.created',
      'command.accepted',
      'text.delta',
      'session.resumed',
    ]);
    expect(await session.close()).toHaveLength(2);
  });

  it('ends iteration after session.closed', async () => {
    const session = await kaana.realtime.connect(open as never);
    const closing = session.close();
    const types: string[] = [];
    for await (const event of session) types.push(event.type);
    await closing;
    expect(types).toEqual(['session.created', 'command.accepted', 'session.closed']);
    await expect(session.resume()).rejects.toBeInstanceOf(KaanaConnectionError);
  });
});
