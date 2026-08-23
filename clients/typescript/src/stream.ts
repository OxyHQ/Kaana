/**
 * Kaana's transport framing, and what each frame decodes to.
 *
 * One connection carries two shapes: the customer-visible event stream and the
 * technical usage record settlement runs against. The `event:` name is what
 * tells them apart, so a consumer never has to guess a shape from its own
 * fields. The names are `internal/httpapi`'s `FrameStreamEvent` and
 * `FrameUsageReport`.
 *
 * The frame NAMES are transport and are not part of the published contract,
 * which is why they are declared here while the payload schemas are imported
 * from `@oxyhq/contracts`. The two layers fail differently on purpose: an
 * unrecognised frame name is surfaced as {@link KaanaUnknownFrame}, because a
 * new frame is an additive transport change; a KNOWN frame whose payload does
 * not parse throws, because that is a contract skew and reading past it means
 * treating an event nobody understands as ordinary output.
 */

import {
  inferenceStreamEventSchema,
  normalizedUsageReportSchema,
  type InferenceStreamEvent,
  type NormalizedUsageReport,
} from '@oxyhq/contracts';

import { KaanaProtocolError } from './errors.js';
import type { SseFrame } from './sse.js';

/** The frame carrying one normalized stream event. */
export const KAANA_FRAME_STREAM_EVENT = 'stream_event';

/** The frame carrying the request's usage report, emitted once, at the end. */
export const KAANA_FRAME_USAGE_REPORT = 'usage_report';

export interface KaanaStreamEventFrame {
  readonly frame: typeof KAANA_FRAME_STREAM_EVENT;
  readonly event: InferenceStreamEvent;
}

export interface KaanaUsageReportFrame {
  readonly frame: typeof KAANA_FRAME_USAGE_REPORT;
  readonly report: NormalizedUsageReport;
}

/**
 * A frame this build has no reading for.
 *
 * Surfaced rather than dropped: a consumer that logs these is how a transport
 * addition becomes visible before anything depends on it, and a consumer that
 * ignores them is unaffected.
 */
export interface KaanaUnknownFrame {
  readonly frame: 'unknown';
  readonly name: string;
  readonly data: string;
}

export type KaanaFrame = KaanaStreamEventFrame | KaanaUsageReportFrame | KaanaUnknownFrame;

function parseJson(frame: SseFrame): unknown {
  try {
    return JSON.parse(frame.data) as unknown;
  } catch (cause) {
    // The payload is not quoted: a frame Kaana could not encode is one of the
    // places an upstream diagnostic — and with it a credential an upstream
    // echoed — would reach a log.
    throw new KaanaProtocolError(`the ${frame.name} frame is not JSON`, { cause });
  }
}

/** Decodes one SSE frame into the shape its name promises. */
export function decodeKaanaFrame(frame: SseFrame): KaanaFrame {
  switch (frame.name) {
    case KAANA_FRAME_STREAM_EVENT: {
      const parsed = inferenceStreamEventSchema.safeParse(parseJson(frame));
      if (!parsed.success) {
        throw new KaanaProtocolError(
          'a stream event does not match the inference contract this build implements',
          { cause: parsed.error },
        );
      }
      return { frame: KAANA_FRAME_STREAM_EVENT, event: parsed.data };
    }
    case KAANA_FRAME_USAGE_REPORT: {
      const parsed = normalizedUsageReportSchema.safeParse(parseJson(frame));
      if (!parsed.success) {
        throw new KaanaProtocolError(
          'the usage report does not match the inference contract this build implements',
          { cause: parsed.error },
        );
      }
      return { frame: KAANA_FRAME_USAGE_REPORT, report: parsed.data };
    }
    default:
      return { frame: 'unknown', name: frame.name, data: frame.data };
  }
}
