import { readFileSync } from 'node:fs';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { describe, expect, it } from 'vitest';
import { render } from './render.js';
import { jsonSink } from './sinks.js';
import { createSpanlog } from './spanlog.js';
import type { Level, SpanEvent } from './types.js';

interface RenderCase {
  name: string;
  options: { utcOffsetMinutes: number };
  event: Omit<SpanEvent, 'time'> & { time: string };
  expect: {
    level: Level;
    msg: string;
    fields: Record<string, unknown>;
    lines: string[];
  };
}

type JsonLine = Record<string, unknown>;

const cases = JSON.parse(
  readFileSync(new URL('../../spec/cases.json', import.meta.url), 'utf8'),
) as { render: RenderCase[] };

const schema = JSON.parse(
  readFileSync(
    new URL('../../spec/log-line.schema.json', import.meta.url),
    'utf8',
  ),
) as object;

// Ajv in strict mode refuses a schema whose format it does not know, and date-time
// needs the separate ajv-formats package. The time format is checked by a regular
// expression below.
const validate = new Ajv2020({ validateFormats: false }).compile(schema);

const UTC_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;

function jsonLineOf(testCase: RenderCase): JsonLine {
  const rendered = render(
    { ...testCase.event, time: new Date(testCase.event.time) },
    testCase.options,
  );
  const written: string[] = [];
  jsonSink((text) => written.push(text)).write(rendered);
  return JSON.parse(written.join('')) as JsonLine;
}

function expectedLineOf(testCase: RenderCase): JsonLine {
  return {
    time: testCase.event.time,
    level: testCase.expect.level,
    ...testCase.expect.fields,
    msg: testCase.expect.msg,
  };
}

function caseNamed(name: string): RenderCase {
  const found = cases.render.find((testCase) => testCase.name === name);
  if (found === undefined) {
    throw new Error(`spec/cases.json has no render case named "${name}"`);
  }
  return found;
}

function errorsOf(line: JsonLine): string {
  validate(line);
  return JSON.stringify(validate.errors);
}

describe('spec/log-line.schema.json', () => {
  it('accepts the JSON line of every render case', () => {
    for (const testCase of cases.render) {
      const line = jsonLineOf(testCase);
      expect(line.time, testCase.name).toMatch(UTC_TIME);
      expect(validate(line), `${testCase.name}: ${errorsOf(line)}`).toBe(true);
    }
  });

  it('accepts the fields of every render case with time, level and msg added', () => {
    for (const testCase of cases.render) {
      const line = expectedLineOf(testCase);
      expect(validate(line), `${testCase.name}: ${errorsOf(line)}`).toBe(true);
    }
  });

  it('accepts the query lines of a statement without a keyword', () => {
    const written: string[] = [];
    const log = createSpanlog({
      service: 'shop',
      level: 'debug',
      sink: jsonSink((text) => written.push(text)),
    });
    log.startQuery('-- comment only').end();

    const lines = written.map((text) => JSON.parse(text) as JsonLine);
    expect(lines.map((line) => line.phase)).toEqual(['START', 'DONE']);
    for (const line of lines) {
      expect(line).not.toHaveProperty('op');
      expect(line).not.toHaveProperty('target');
      expect(validate(line), `${line.phase}: ${errorsOf(line)}`).toBe(true);
    }
  });

  it('rejects lines that break the field rules', () => {
    const requestStart = expectedLineOf(caseNamed('request start'));
    const requestDone = expectedLineOf(caseNamed('request done with status'));
    const runStart = expectedLineOf(caseNamed('run start'));
    const queryDone = expectedLineOf(
      caseNamed('query done at debug with tables'),
    );
    const callFailed = expectedLineOf(
      caseNamed('first report of an error carries the stack'),
    );
    for (const valid of [
      requestStart,
      requestDone,
      runStart,
      queryDone,
      callFailed,
    ]) {
      expect(validate(valid), errorsOf(valid)).toBe(true);
    }

    const { status: _status, ...requestWithoutStatus } = requestDone;
    const { tables: _tables, ...queryWithoutTables } = queryDone;
    const broken: Record<string, JsonLine> = {
      'an unknown field': { ...requestDone, extra: 1 },
      'scope and method on a request': {
        ...requestStart,
        scope: 'OrderService',
        method: 'handleOrder',
      },
      'ms on START': { ...runStart, ms: 1 },
      'request DONE without status': requestWithoutStatus,
      'query without tables': queryWithoutTables,
      'err without message': { ...callFailed, err: { name: 'Error' } },
    };
    for (const [rule, line] of Object.entries(broken)) {
      expect(validate(line), rule).toBe(false);
    }
  });
});
