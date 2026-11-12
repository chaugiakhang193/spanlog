import { createSpanlog, type SpanlogOptions } from '../spanlog.js';
import type { Rendered } from '../types.js';

export const MS = /\d+(\.\d+)?ms/;

/** setup returns a Spanlog at the info level that keeps every line in written. */
export function setup(options: Partial<SpanlogOptions> = {}) {
  const written: Rendered[] = [];
  const spanlog = createSpanlog({
    service: 'shop',
    sink: { write: (rendered) => written.push(rendered) },
    utcOffsetMinutes: 0,
    ...options,
  });
  return { spanlog, written };
}

/** setupAtDebug is setup at the debug level, unless options set another level. */
export function setupAtDebug(options: Partial<SpanlogOptions> = {}) {
  return setup({ level: 'debug', ...options });
}

// The shapes hide the measured duration, which changes from run to run.

export function shape(rendered: Rendered): string {
  return rendered.msg.replace(MS, 'Nms');
}

export function shapeWithLevel(rendered: Rendered): string {
  return `${rendered.level} ${shape(rendered)}`;
}

export function shapeWithRequest(rendered: Rendered): string {
  const reqId = String(rendered.fields.reqId ?? '-');
  return `${reqId} ${shapeWithLevel(rendered)}`;
}

export function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
