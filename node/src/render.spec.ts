import { describe, expect, it } from 'vitest';
import { formatTime, render, stableJson } from './render.js';
import type { ErrorInfo } from './types.js';

const AT = new Date('2026-10-24T14:15:03.120Z');

describe('stableJson', () => {
  it('sorts integer-like keys as strings', () => {
    expect(stableJson({ b: 1, 10: 2, 2: 3 })).toBe('{"10":2,"2":3,"b":1}');
  });

  it('drops undefined properties and writes undefined array items as null', () => {
    expect(stableJson({ a: undefined, b: [undefined, 1] })).toBe(
      '{"b":[null,1]}',
    );
  });

  it('writes a bigint as a string and a date through toJSON', () => {
    expect(stableJson({ id: 12n, at: AT })).toBe(
      '{"at":"2026-10-24T14:15:03.120Z","id":"12"}',
    );
  });
});

describe('formatTime', () => {
  it('moves to the previous day for a negative offset', () => {
    expect(formatTime(new Date('2026-01-01T02:00:00.005Z'), -300)).toBe(
      '2025-12-31 21:00:00.005 -05:00',
    );
  });
});

describe('render', () => {
  it('skips blank lines of a stack', () => {
    const rendered = render(
      {
        time: AT,
        service: 'shop',
        scope: 'Mailer',
        method: 'send',
        action: 'run',
        phase: 'FAILED',
        durationMs: 1,
        firstReport: true,
        err: { name: 'Error', message: 'x', stack: 'Error: x\n\n    at a\n' },
      },
      { utcOffsetMinutes: 0 },
    );

    expect(rendered.lines.slice(1)).toEqual(['    at a']);
  });

  it('applies stackFilter to the readable lines and leaves err.stack whole', () => {
    const stack = 'Error: x\n    at a\n    at b';
    const rendered = render(
      {
        time: AT,
        service: 'shop',
        scope: 'Mailer',
        method: 'send',
        action: 'run',
        phase: 'FAILED',
        durationMs: 1,
        firstReport: true,
        err: { name: 'Error', message: 'x', stack },
      },
      {
        utcOffsetMinutes: 0,
        stackFilter: (lines) => lines.filter((line) => line !== 'at b'),
      },
    );

    expect(rendered.lines.slice(1)).toEqual(['    at a']);
    expect((rendered.fields.err as ErrorInfo).stack).toBe(stack);
  });

  it('shows a dash for a request end without status', () => {
    const rendered = render(
      {
        time: AT,
        service: 'shop',
        action: 'request',
        target: 'GET /health',
        phase: 'DONE',
        durationMs: 1,
      },
      { utcOffsetMinutes: 0 },
    );

    expect(rendered.msg).toBe('GET /health | DONE - 1ms');
  });
});
