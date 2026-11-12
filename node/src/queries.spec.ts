import { describe, expect, it } from 'vitest';
import { delay, setupAtDebug, shapeWithLevel } from './testing/helpers.js';

interface Database {
  run(sql: string): Promise<void>;
}

// db lives on its own object. traced wraps async functions held by the
// repository itself, and the statement must run inside reserve's frame.
class InventoryRepository {
  constructor(private readonly db: Database) {}

  async reserve(): Promise<void> {
    await this.db.run('UPDATE "warehouse"."inventory" SET "available" = 0');
  }
}

describe('startQuery', () => {
  it('belongs to the frame that runs the statement', async () => {
    const { spanlog, written } = setupAtDebug();
    const repository = spanlog.traced(
      new InventoryRepository({
        run: async (sql) => {
          const span = spanlog.startQuery(sql);
          await delay(1);
          span.end();
        },
      }),
      'InventoryRepository',
    );

    await spanlog.withRequest('7f3a9c', () => repository.reserve());

    expect(written.map(shapeWithLevel)).toEqual([
      'info InventoryRepository.reserve - run | START',
      'debug InventoryRepository.reserve - query UPDATE warehouse.inventory | START',
      'debug InventoryRepository.reserve - query UPDATE warehouse.inventory | DONE Nms',
      'info InventoryRepository.reserve - run | DONE Nms',
    ]);
    expect(written[2]?.fields).toMatchObject({
      reqId: '7f3a9c',
      scope: 'InventoryRepository',
      method: 'reserve',
      action: 'query',
      op: 'UPDATE',
      tables: ['warehouse.inventory'],
    });
  });

  it('never writes the statement text', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.startQuery("SELECT * FROM users WHERE email = 'a@b.c'").end();

    const everything = JSON.stringify(written);
    expect(everything).not.toContain('a@b.c');
    expect(everything).not.toContain('WHERE');
  });

  it('keeps a string literal out of the tables', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog
      .startQuery("INSERT INTO notes (body) VALUES ('sent from alice_secret')")
      .end();

    expect(written[0]?.fields.tables).toEqual(['notes']);
    expect(JSON.stringify(written)).not.toContain('alice_secret');
  });

  it('never writes the statement text when the statement fails', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog
      .startQuery("SELECT * FROM users WHERE email = 'a@b.c'")
      .fail(new Error('boom'));

    const everything = JSON.stringify(written);
    expect(everything).not.toContain('a@b.c');
    expect(everything).not.toContain('WHERE');
  });

  it('writes no frame outside a wrapped method', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.startQuery('COMMIT').end();

    expect(written.map(shapeWithLevel)).toEqual([
      'debug query COMMIT | START',
      'debug query COMMIT | DONE Nms',
    ]);
    expect(written[0]?.lines[0]).toContain('req=-');
  });

  it('ends a ROLLBACK at warn', () => {
    const { spanlog, written } = setupAtDebug({ level: 'info' });
    spanlog.startQuery('ROLLBACK').end();

    expect(written.map(shapeWithLevel)).toEqual([
      'warn query ROLLBACK | DONE Nms',
    ]);
  });

  it('names the tables an adapter gives a ROLLBACK', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.startQuery('ROLLBACK', ['orders', 'warehouse.inventory']).end();

    expect(written.map(shapeWithLevel)).toEqual([
      'debug query ROLLBACK orders, warehouse.inventory | START',
      'warn query ROLLBACK orders, warehouse.inventory | DONE Nms',
    ]);
    expect(written[1]?.fields.tables).toEqual([
      'orders',
      'warehouse.inventory',
    ]);
  });

  it('keeps the tables of its text for a statement other than ROLLBACK', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.startQuery('COMMIT', ['orders']).end();

    expect(written.map(shapeWithLevel)).toEqual([
      'debug query COMMIT | START',
      'debug query COMMIT | DONE Nms',
    ]);
  });

  it('fails at warn without a stack and leaves the error to its frame', async () => {
    const { spanlog, written } = setupAtDebug();
    const failure = new Error('duplicate key value');
    const repository = spanlog.traced(
      new InventoryRepository({
        run: async (sql) => {
          const span = spanlog.startQuery(sql);
          span.fail(failure);
          throw failure;
        },
      }),
      'InventoryRepository',
    );

    await expect(
      spanlog.withRequest('b81e44', () => repository.reserve()),
    ).rejects.toBe(failure);

    expect(written.map(shapeWithLevel)).toEqual([
      'info InventoryRepository.reserve - run | START',
      'debug InventoryRepository.reserve - query UPDATE warehouse.inventory | START',
      'warn InventoryRepository.reserve - query UPDATE warehouse.inventory | FAILED Nms Error: duplicate key value',
      'error InventoryRepository.reserve - run | FAILED Nms Error: duplicate key value',
    ]);
    expect(written[2]?.lines).toHaveLength(1);
    expect(written[2]?.fields.err).toEqual({
      name: 'Error',
      message: 'duplicate key value',
    });
    expect(written[3]?.lines.length).toBeGreaterThan(1);
    expect(written[3]?.fields.err).toHaveProperty('stack');
  });

  it('writes only the first end line', () => {
    const { spanlog, written } = setupAtDebug();
    const span = spanlog.startQuery('COMMIT');
    span.end();
    span.fail(new Error('late'));

    expect(written).toHaveLength(2);
  });

  it('ends with SLOW after slowMs', async () => {
    const { spanlog, written } = setupAtDebug({ slowMs: 5 });
    const span = spanlog.startQuery('SELECT * FROM orders');
    await delay(15);
    span.end();

    expect(shapeWithLevel(written[1]!)).toBe(
      'warn query SELECT orders | SLOW Nms',
    );
  });
});
