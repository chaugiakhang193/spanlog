import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { queryTarget, summarizeSql } from './sql.js';

interface SqlCase {
  name: string;
  sql: string;
  op: string;
  tables: string[];
}

const cases = JSON.parse(
  readFileSync(new URL('../../spec/cases.json', import.meta.url), 'utf8'),
) as { sql: SqlCase[] };

describe('spec/cases.json sql cases', () => {
  it('loads the shared cases', () => {
    expect(cases.sql.length).toBeGreaterThanOrEqual(75);
  });

  for (const testCase of cases.sql) {
    it(testCase.name, () => {
      expect(summarizeSql(testCase.sql)).toEqual({
        op: testCase.op,
        tables: testCase.tables,
      });
    });
  }
});

describe('summarizeSql', () => {
  it('keeps a doubled quote inside a quoted name', () => {
    expect(summarizeSql('SELECT 1 FROM "odd""name"').tables).toEqual([
      'odd"name',
    ]);
  });

  it('lists a table once when it appears twice', () => {
    const sql = 'SELECT a.id FROM orders a JOIN orders b ON b.parent_id = a.id';
    expect(summarizeSql(sql).tables).toEqual(['orders']);
  });

  it('returns an empty op for text that is not a statement', () => {
    expect(summarizeSql('')).toEqual({ op: '', tables: [] });
    expect(summarizeSql('  $1 ')).toEqual({ op: '', tables: [] });
  });

  it('survives an unclosed quote and parenthesis', () => {
    expect(summarizeSql('SELECT * FROM "orders')).toEqual({
      op: 'SELECT',
      tables: ['orders'],
    });
    expect(summarizeSql('WITH x AS (SELECT 1').op).toBe('');
  });

  it('survives an unclosed string, comment and dollar quote', () => {
    expect(summarizeSql("SELECT 'from alice FROM orders").tables).toEqual([]);
    expect(summarizeSql('SELECT /* from alice FROM orders').tables).toEqual([]);
    expect(summarizeSql('SELECT $$ from alice FROM orders').tables).toEqual([]);
  });

  it('keeps a name that holds a dollar sign', () => {
    expect(summarizeSql('SELECT 1 FROM a$b$c').tables).toEqual(['a$b$c']);
  });

  it('does not read a quoted name as a parenthesis', () => {
    const sql = 'WITH a AS (SELECT "(" FROM items) SELECT * FROM a';
    expect(summarizeSql(sql)).toEqual({ op: 'SELECT', tables: ['items'] });
  });

  // A scan of every scope for each name takes over 10 s at this depth with one
  // name. The limit leaves room for a loaded machine.
  it.each([
    ['different names', (level: number) => `c${level}`],
    ['one name', () => 'c'],
  ])(
    'reads deeply nested common table expressions with %s',
    (_label, name) => {
      let sql = 'SELECT * FROM orders';
      for (let level = 0; level < 128_000; level += 1) {
        sql = `WITH ${name(level)} AS (${sql}) SELECT * FROM ${name(level)}`;
      }

      const start = performance.now();
      expect(summarizeSql(sql).tables).toEqual(['orders']);
      expect(performance.now() - start).toBeLessThan(5_000);
    },
    30_000,
  );

  // A scan of the tables seen so far for each name takes over 30 s at this count.
  it('reads a statement that names many different tables', () => {
    const names = Array.from({ length: 128_000 }, (_, index) => `t${index}`);
    const sql = `SELECT * FROM ${names.join(' JOIN ')}`;

    const start = performance.now();
    expect(summarizeSql(sql).tables).toEqual(names);
    expect(performance.now() - start).toBeLessThan(5_000);
  }, 60_000);
});

describe('queryTarget', () => {
  it('joins op and tables', () => {
    expect(queryTarget({ op: 'SELECT', tables: ['orders', 'items'] })).toBe(
      'SELECT orders, items',
    );
  });

  it('is the op alone without tables', () => {
    expect(queryTarget({ op: 'COMMIT', tables: [] })).toBe('COMMIT');
  });
});
