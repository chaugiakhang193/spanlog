import { describe, expect, it, vi } from 'vitest';
import { jsonSink, pinoSink, prettySink } from './sinks.js';
import type { Rendered } from './types.js';

const RENDERED: Rendered = {
  time: new Date('2026-10-24T14:15:03.120Z'),
  level: 'error',
  msg: 'send | FAILED 2ms Error: x',
  fields: { service: 'shop', phase: 'FAILED', ms: 2, params: { id: 7n } },
  lines: ['main line', '    at a'],
};

describe('prettySink', () => {
  it('writes the readable lines of one event in one write', () => {
    const writeText = vi.fn();

    prettySink(writeText).write(RENDERED);

    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith('main line\n    at a\n');
  });
});

describe('jsonSink', () => {
  it('writes time, level, fields and msg in that order', () => {
    const writeText = vi.fn();

    jsonSink(writeText).write(RENDERED);

    expect(writeText).toHaveBeenCalledWith(
      '{"time":"2026-10-24T14:15:03.120Z","level":"error","service":"shop",' +
        '"phase":"FAILED","ms":2,"params":{"id":"7"},' +
        '"msg":"send | FAILED 2ms Error: x"}\n',
    );
  });
});

describe('pinoSink', () => {
  it('calls the method named after the level with fields and msg', () => {
    const logger = {
      debug: vi.fn(),
      info: vi.fn(),
      warn: vi.fn(),
      error: vi.fn(),
    };

    pinoSink(logger).write(RENDERED);

    expect(logger.error).toHaveBeenCalledWith(RENDERED.fields, RENDERED.msg);
    expect(logger.info).not.toHaveBeenCalled();
  });
});
