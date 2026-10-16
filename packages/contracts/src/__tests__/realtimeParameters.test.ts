import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
  checkRealtimeSessionOpen,
  type RealtimeGovernedFields,
  realtimeParameterIssues,
  realtimeParametersSchema,
  realtimeResponseParametersSchema,
  realtimeSessionConfigUpdateSchema,
  realtimeSessionOpenSchema,
} from '../index.js';

/**
 * internal/contract/testdata/realtime-parameter-issues.json: the vectors Go's
 * `RealtimeParameters.Issues` is held to as well, so the published check and
 * Kaana's own cannot disagree about one session.
 */
const vectors = JSON.parse(
  readFileSync(
    fileURLToPath(
      new URL(
        '../../../../internal/contract/testdata/realtime-parameter-issues.json',
        import.meta.url,
      ),
    ),
    'utf8',
  ),
) as {
  statements: Record<string, unknown>;
  vectors: {
    name: string;
    parameters: string;
    prefix: 'config' | 'response';
    fields: unknown;
    params: string[];
  }[];
};

describe('realtimeParameterIssues', () => {
  const statements = Object.fromEntries(
    Object.entries(vectors.statements).map(([name, raw]) => [
      name,
      realtimeParametersSchema.parse(raw),
    ]),
  );

  for (const vector of vectors.vectors) {
    it(vector.name, () => {
      const parameters = statements[vector.parameters];
      if (parameters === undefined) throw new Error(`no statement ${vector.parameters}`);
      const fields: RealtimeGovernedFields =
        vector.prefix === 'config'
          ? realtimeSessionConfigUpdateSchema.parse(vector.fields)
          : realtimeResponseParametersSchema.parse(vector.fields);
      const issues = realtimeParameterIssues(vector.prefix, fields, parameters);
      expect(issues.map((issue) => issue.param)).toEqual(vector.params);
    });
  }

  it('applies the parameters to a session open', () => {
    const open = realtimeSessionOpenSchema.parse({
      type: 'session.open',
      kind: 'conversation',
      config: {
        outputModalities: ['text'],
        voice: 'eve',
        inputAudioFormat: 'pcm16_24khz',
        turnDetection: { type: 'none' },
      },
    });
    const issues = checkRealtimeSessionOpen(open, {
      capabilities: {
        inputModalities: ['text', 'audio'],
        outputModalities: ['text', 'audio'],
        tools: true,
        realtime: {
          transports: ['websocket'],
          sessionKinds: ['conversation'],
          parameters: statements.xai,
        },
      } as never,
    });
    expect(issues.map((issue) => issue.param)).toEqual(['config.outputModalities']);
  });
});
