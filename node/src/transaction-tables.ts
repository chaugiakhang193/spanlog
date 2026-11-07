import {
  chainsTransaction,
  rollsBackToSavepoint,
  summarizeSql,
} from './sql.js';

const BEGIN_OPS = new Set(['BEGIN', 'START TRANSACTION']);
// ABORT is a ROLLBACK in Postgres. Its line keeps the op ABORT. That line stays
// at debug and names no table.
const END_OPS = new Set(['COMMIT', 'END', 'ABORT']);
// A write inside a common table expression of a SELECT is not seen, because
// the op of that statement is SELECT.
const WRITE_OPS = new Set(['INSERT', 'UPDATE', 'DELETE', 'MERGE']);

/**
 * TransactionTables follows the statements of one connection and keeps the
 * tables that its open transaction has written, so that a ROLLBACK line can
 * name them. The rules are those of the query lines in docs/log-format.md.
 */
export class TransactionTables {
  // levels[0] holds the writes of the transaction, and each later entry those
  // of one savepoint, innermost last. ROLLBACK TO and RELEASE act on the
  // innermost savepoint, whatever name they give, as TypeORM and SQLAlchemy
  // nest them.
  private levels: string[][] = [];

  /**
   * statementStarting runs before a statement. For a ROLLBACK it returns the
   * tables that the rollback undoes. For any other statement it returns
   * undefined. A statement that ends the transaction ends it here, because
   * Postgres also ends a transaction whose COMMIT fails.
   */
  statementStarting(sql: string): string[] | undefined {
    const { op } = summarizeSql(sql);
    if (BEGIN_OPS.has(op)) {
      this.levels = [[]];
    } else if (END_OPS.has(op)) {
      this.levels = [];
    } else if (op === 'SAVEPOINT') {
      this.openSavepoint();
    } else if (op === 'RELEASE') {
      this.releaseSavepoint();
    } else if (op === 'ROLLBACK') {
      return rollsBackToSavepoint(sql)
        ? this.rollBackToSavepoint()
        : this.rollBack();
    }
    return undefined;
  }

  /**
   * statementDone runs after a statement that ended without an error. A
   * statement with AND CHAIN opens the next transaction here, because Postgres
   * opens it only when that statement succeeds.
   */
  statementDone(sql: string): void {
    const { op, tables } = summarizeSql(sql);
    const innermost = this.levels.at(-1);
    if (WRITE_OPS.has(op) && innermost !== undefined) {
      addNew(innermost, tables);
    } else if (
      (END_OPS.has(op) || op === 'ROLLBACK') &&
      chainsTransaction(sql)
    ) {
      this.levels = [[]];
    }
  }

  private rollBack(): string[] {
    const undone: string[] = [];
    for (const level of this.levels) {
      addNew(undone, level);
    }
    this.levels = [];
    return undone;
  }

  // Postgres keeps the savepoint open after ROLLBACK TO, with nothing written
  // since.
  private rollBackToSavepoint(): string[] {
    if (this.levels.length < 2) {
      return [];
    }
    const innermost = this.levels.length - 1;
    const undone = this.levels[innermost] ?? [];
    this.levels[innermost] = [];
    return undone;
  }

  private openSavepoint(): void {
    if (this.levels.length > 0) {
      this.levels.push([]);
    }
  }

  private releaseSavepoint(): void {
    if (this.levels.length < 2) {
      return;
    }
    const released = this.levels.pop() ?? [];
    addNew(this.levels[this.levels.length - 1] ?? [], released);
  }
}

function addNew(into: string[], tables: readonly string[]): void {
  for (const table of tables) {
    if (!into.includes(table)) {
      into.push(table);
    }
  }
}
