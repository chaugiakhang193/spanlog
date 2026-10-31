import type { ErrorInfo, Level, Rendered, SpanEvent } from './types.js';

export interface RenderOptions {
  /** Offset of the readable line from UTC in minutes, positive east of UTC. */
  utcOffsetMinutes: number;
  /**
   * Picks the stack lines shown under the readable line. It receives the
   * trimmed, non-empty lines after the first one. err.stack in the fields
   * keeps every line.
   */
  stackFilter?: (lines: string[]) => string[];
}

const LEVEL_WIDTH = 5;
const STACK_INDENT = '    ';

/**
 * render turns an event into its level, message, structured fields and
 * readable lines. It is a pure function. spec/cases.json pins its output.
 */
export function render(event: SpanEvent, options: RenderOptions): Rendered {
  const level = levelFor(event);
  const frame = frameOf(event);
  const tail = tailOf(event);
  return {
    time: event.time,
    level,
    msg: frame === undefined ? tail : `${frame} - ${tail}`,
    fields: fieldsOf(event),
    lines: [
      readableLine(event, level, frame, tail, options.utcOffsetMinutes),
      ...stackLines(event, options.stackFilter),
    ],
  };
}

export function levelFor(event: SpanEvent): Level {
  switch (event.phase) {
    case 'START':
      return baseLevel(event);
    case 'DONE':
      if (event.action === 'query' && event.op === 'ROLLBACK') {
        return 'warn';
      }
      return baseLevel(event);
    case 'SLOW':
    case 'ABORTED':
      return 'warn';
    case 'FAILED':
      if (event.action === 'request' || event.action === 'query') {
        return 'warn';
      }
      return event.firstReport === true ? 'error' : baseLevel(event);
  }
}

function baseLevel(event: SpanEvent): Level {
  if (event.action === 'query') {
    return 'debug';
  }
  if (event.action === 'call' && event.sameScope === true) {
    return 'debug';
  }
  return 'info';
}

function frameOf(event: SpanEvent): string | undefined {
  if (!event.scope || !event.method) {
    return undefined;
  }
  return `${event.scope}.${event.method}`;
}

// tailOf is the part that msg and the readable line share: body, phase and
// detail. msg puts the frame in front of it, the readable line puts the frame
// in brackets.
function tailOf(event: SpanEvent): string {
  const body = bodyOf(event);
  const detail = detailOf(event);
  if (detail === '') {
    return `${body} | ${event.phase}`;
  }
  return `${body} | ${event.phase} ${detail}`;
}

function bodyOf(event: SpanEvent): string {
  const target = event.target ?? '';
  switch (event.action) {
    case 'request':
      return target;
    case 'run':
      return 'run';
    case 'call':
      return `call ${target}`;
    case 'query':
      return `query ${target}`;
  }
}

function detailOf(event: SpanEvent): string {
  const ms =
    event.durationMs === undefined ? '' : `${roundMs(event.durationMs)}ms`;
  if (event.action === 'request') {
    switch (event.phase) {
      case 'START':
        return '';
      case 'ABORTED':
        return ms;
      default:
        return `${event.status ?? '-'} ${ms}`;
    }
  }
  switch (event.phase) {
    case 'START':
      return event.params === undefined ? '' : stableJson(event.params);
    case 'DONE':
    case 'SLOW':
      return event.summary === undefined
        ? ms
        : `${ms} ${stableJson(event.summary)}`;
    case 'FAILED':
      return event.err === undefined ? ms : `${ms} ${errorText(event.err)}`;
    case 'ABORTED':
      return ms;
  }
}

function errorText(err: ErrorInfo): string {
  return err.name ? `${err.name}: ${err.message}` : err.message;
}

function fieldsOf(event: SpanEvent): Record<string, unknown> {
  const fields: Record<string, unknown> = { service: event.service };
  if (event.reqId) fields.reqId = event.reqId;
  if (event.scope) fields.scope = event.scope;
  if (event.method) fields.method = event.method;
  fields.action = event.action;
  if (event.target) fields.target = event.target;
  fields.phase = event.phase;
  if (event.durationMs !== undefined) fields.ms = roundMs(event.durationMs);
  if (event.status !== undefined) fields.status = event.status;
  if (event.op) fields.op = event.op;
  if (event.tables !== undefined) fields.tables = event.tables;
  if (event.err !== undefined) fields.err = errorFields(event, event.err);
  if (event.params !== undefined) fields.params = event.params;
  if (event.summary !== undefined) fields.summary = event.summary;
  return fields;
}

function errorFields(event: SpanEvent, err: ErrorInfo): ErrorInfo {
  const fields: ErrorInfo = err.name
    ? { name: err.name, message: err.message }
    : { message: err.message };
  if (isFirstReportOnFrameLine(event) && err.stack !== undefined) {
    fields.stack = err.stack;
  }
  return fields;
}

// Only the first report of an error on a frame line carries the stack. Frames
// further out and failed SQL statements repeat the name and message only.
function isFirstReportOnFrameLine(event: SpanEvent): boolean {
  return (
    event.firstReport === true &&
    (event.action === 'run' || event.action === 'call')
  );
}

function readableLine(
  event: SpanEvent,
  level: Level,
  frame: string | undefined,
  tail: string,
  utcOffsetMinutes: number,
): string {
  const owner =
    frame === undefined
      ? `[${event.service}]`
      : `[${event.service}] [${frame}]`;
  const label = level.toUpperCase().padEnd(LEVEL_WIDTH);
  return `${formatTime(event.time, utcOffsetMinutes)} ${label} ${owner} req=${event.reqId || '-'} ${tail}`;
}

// The first line of a V8 stack repeats name and message, which the main line
// already shows.
function stackLines(
  event: SpanEvent,
  stackFilter: RenderOptions['stackFilter'],
): string[] {
  const stack = event.err?.stack;
  if (!isFirstReportOnFrameLine(event) || stack === undefined) {
    return [];
  }
  const lines = stack
    .split('\n')
    .slice(1)
    .map((line) => line.trim())
    .filter((line) => line !== '');
  const shown = stackFilter === undefined ? lines : stackFilter(lines);
  return shown.map((line) => `${STACK_INDENT}${line}`);
}

export function formatTime(time: Date, utcOffsetMinutes: number): string {
  const shifted = new Date(time.getTime() + utcOffsetMinutes * 60_000);
  const date = [
    shifted.getUTCFullYear(),
    pad(shifted.getUTCMonth() + 1),
    pad(shifted.getUTCDate()),
  ].join('-');
  const clock = [
    pad(shifted.getUTCHours()),
    pad(shifted.getUTCMinutes()),
    pad(shifted.getUTCSeconds()),
  ].join(':');
  const millis = pad(shifted.getUTCMilliseconds(), 3);
  return `${date} ${clock}.${millis} ${formatOffset(utcOffsetMinutes)}`;
}

function formatOffset(minutes: number): string {
  const sign = minutes < 0 ? '-' : '+';
  const abs = Math.abs(minutes);
  return `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

function pad(value: number, width = 2): string {
  return String(value).padStart(width, '0');
}

export function roundMs(ms: number): number {
  return Math.round(ms * 10) / 10;
}

/**
 * stableJson encodes a value as compact JSON with object keys sorted at every
 * level. Keys are written in sorted order directly, because a JavaScript
 * object puts integer-like keys first whatever order they are added in.
 * A bigint is written as a string.
 */
export function stableJson(value: unknown): string {
  return encode(value) ?? 'null';
}

function encode(value: unknown): string | undefined {
  if (typeof value === 'bigint') {
    return JSON.stringify(value.toString());
  }
  if (Array.isArray(value)) {
    return `[${value.map((item) => encode(item) ?? 'null').join(',')}]`;
  }
  if (isObjectWithoutToJSONMethod(value)) {
    const parts: string[] = [];
    for (const key of Object.keys(value).sort()) {
      const encoded = encode(value[key]);
      if (encoded !== undefined) {
        parts.push(`${JSON.stringify(key)}:${encoded}`);
      }
    }
    return `{${parts.join(',')}}`;
  }
  // JSON.stringify returns undefined for undefined, functions and symbols.
  return JSON.stringify(value) as string | undefined;
}

function isObjectWithoutToJSONMethod(
  value: unknown,
): value is Record<string, unknown> {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as { toJSON?: unknown }).toJSON !== 'function'
  );
}
