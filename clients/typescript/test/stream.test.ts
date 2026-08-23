/**
 * Frame decoding: which failures are fatal and which are additive.
 *
 * Both directions matter. A stream event that does not parse must throw,
 * because an event a consumer silently drops is either output the customer paid
 * for and never saw or a terminal event that terminated nothing — the same
 * reason `internal/contract` has no generic event type. An unrecognised FRAME
 * NAME must not throw, because the frame names are transport rather than
 * contract and a new one is an additive change.
 */

import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { KaanaProtocolError } from '../src/errors.js';
import {
  decodeKaanaFrame,
  KAANA_FRAME_STREAM_EVENT,
  KAANA_FRAME_USAGE_REPORT,
} from '../src/stream.js';
import { DONE_EVENT, ROUTE_SWITCH_EVENT, START_EVENT, USAGE_REPORT } from './fixtures.js';

describe('decodeKaanaFrame', () => {
  it('reads a stream_event frame as the contract event it carries', () => {
    const frame = decodeKaanaFrame({
      name: KAANA_FRAME_STREAM_EVENT,
      data: JSON.stringify(START_EVENT),
    });

    assert.deepEqual(frame, { frame: KAANA_FRAME_STREAM_EVENT, event: START_EVENT });
  });

  it('reads a deployment-scope route switch, detail and all', () => {
    const frame = decodeKaanaFrame({
      name: KAANA_FRAME_STREAM_EVENT,
      data: JSON.stringify(ROUTE_SWITCH_EVENT),
    });

    assert.deepEqual(frame, { frame: KAANA_FRAME_STREAM_EVENT, event: ROUTE_SWITCH_EVENT });
  });

  it('reads a usage_report frame as the usage record', () => {
    const frame = decodeKaanaFrame({
      name: KAANA_FRAME_USAGE_REPORT,
      data: JSON.stringify(USAGE_REPORT),
    });

    assert.deepEqual(frame, { frame: KAANA_FRAME_USAGE_REPORT, report: USAGE_REPORT });
  });

  it('tolerates a field a newer producer added, which is additive by contract', () => {
    const frame = decodeKaanaFrame({
      name: KAANA_FRAME_STREAM_EVENT,
      data: JSON.stringify({ ...DONE_EVENT, somethingNew: 'ignored' }),
    });

    assert.deepEqual(frame, { frame: KAANA_FRAME_STREAM_EVENT, event: DONE_EVENT });
  });

  it('surfaces an unknown frame name instead of dropping or throwing on it', () => {
    const frame = decodeKaanaFrame({ name: 'receipt', data: '{"id":"x"}' });

    assert.deepEqual(frame, { frame: 'unknown', name: 'receipt', data: '{"id":"x"}' });
  });

  it('refuses a known frame whose payload is not JSON', () => {
    assert.throws(
      () => decodeKaanaFrame({ name: KAANA_FRAME_STREAM_EVENT, data: '<html>502</html>' }),
      KaanaProtocolError,
    );
  });

  it('refuses a stream event whose type this build has no reading for', () => {
    assert.throws(
      () =>
        decodeKaanaFrame({
          name: KAANA_FRAME_STREAM_EVENT,
          data: JSON.stringify({ ...START_EVENT, type: 'checkpoint' }),
        }),
      KaanaProtocolError,
    );
  });

  it('refuses a stream event from a schemaVersion this build does not implement', () => {
    assert.throws(
      () =>
        decodeKaanaFrame({
          name: KAANA_FRAME_STREAM_EVENT,
          data: JSON.stringify({ ...START_EVENT, schemaVersion: 2 }),
        }),
      KaanaProtocolError,
    );
  });

  it('refuses a usage report missing a field settlement depends on', () => {
    const { servingProvider: _dropped, ...withoutProvider } = USAGE_REPORT;

    assert.throws(
      () =>
        decodeKaanaFrame({
          name: KAANA_FRAME_USAGE_REPORT,
          data: JSON.stringify(withoutProvider),
        }),
      KaanaProtocolError,
    );
  });

  it('does not quote the payload in the error, which may carry an upstream diagnostic', () => {
    let thrown: Error | null = null;
    try {
      decodeKaanaFrame({
        name: KAANA_FRAME_STREAM_EVENT,
        data: 'Authorization: Bearer sk-live-abcdefghijklmnop',
      });
    } catch (error) {
      thrown = error as Error;
    }

    assert.ok(thrown instanceof KaanaProtocolError);
    assert.equal(thrown.message.includes('sk-live-abcdefghijklmnop'), false);
  });
});
