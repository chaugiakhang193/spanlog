import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { render } from './render.js';
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

const cases = JSON.parse(
  readFileSync(new URL('../../spec/cases.json', import.meta.url), 'utf8'),
) as { render: RenderCase[] };

describe('spec/cases.json render cases', () => {
  it('loads the shared cases', () => {
    expect(cases.render.length).toBeGreaterThanOrEqual(20);
  });

  for (const testCase of cases.render) {
    it(testCase.name, () => {
      const rendered = render(
        { ...testCase.event, time: new Date(testCase.event.time) },
        testCase.options,
      );

      expect(rendered.level).toBe(testCase.expect.level);
      expect(rendered.msg).toBe(testCase.expect.msg);
      expect(rendered.fields).toEqual(testCase.expect.fields);
      expect(rendered.lines).toEqual(testCase.expect.lines);
    });
  }
});
