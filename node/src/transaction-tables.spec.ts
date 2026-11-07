import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { TransactionTables } from './transaction-tables.js';

interface TransactionCase {
  name: string;
  statements: string[];
  rollbacks: string[][];
}

const cases = JSON.parse(
  readFileSync(new URL('../../spec/cases.json', import.meta.url), 'utf8'),
) as { transactions: TransactionCase[] };

// rollbacksOf runs statements the way an adapter does, statementStarting
// before each one and statementDone after it.
function rollbacksOf(statements: readonly string[]): string[][] {
  const transaction = new TransactionTables();
  const rollbacks: string[][] = [];
  for (const sql of statements) {
    const undone = transaction.statementStarting(sql);
    if (undone !== undefined) {
      rollbacks.push(undone);
    }
    transaction.statementDone(sql);
  }
  return rollbacks;
}

describe('spec/cases.json transaction cases', () => {
  it('loads the shared cases', () => {
    expect(cases.transactions.length).toBeGreaterThanOrEqual(25);
  });

  for (const testCase of cases.transactions) {
    it(testCase.name, () => {
      expect(rollbacksOf(testCase.statements)).toEqual(testCase.rollbacks);
    });
  }
});

describe('TransactionTables with a failed statement', () => {
  // A failed statement gets statementStarting and no statementDone, as the
  // adapters call statementDone only after a statement without an error.
  it('does not keep the tables of a failed write', () => {
    const transaction = new TransactionTables();
    transaction.statementStarting('BEGIN');
    transaction.statementDone('BEGIN');
    transaction.statementStarting("INSERT INTO a (v) VALUES ('x')");
    expect(transaction.statementStarting('ROLLBACK')).toEqual([]);
  });

  it('opens no transaction after a failed COMMIT AND CHAIN', () => {
    const transaction = new TransactionTables();
    for (const sql of ['BEGIN', 'INSERT INTO a (v) VALUES (1)']) {
      transaction.statementStarting(sql);
      transaction.statementDone(sql);
    }
    transaction.statementStarting('COMMIT AND CHAIN');
    transaction.statementStarting('INSERT INTO b (v) VALUES (1)');
    transaction.statementDone('INSERT INTO b (v) VALUES (1)');
    expect(transaction.statementStarting('ROLLBACK')).toEqual([]);
  });
});
