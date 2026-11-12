import { AsyncLocalStorage } from 'node:async_hooks';
import {
  DataSource,
  EntitySchema,
  type EntitySubscriberInterface,
  type QueryRunner,
} from 'typeorm';
import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest';
import { createSpanlog, type Spanlog } from './spanlog.js';
import { shapeWithLevel } from './testing/helpers.js';
import { logQueries } from './typeorm.js';
import type { Rendered } from './types.js';

// The database is started by compose.yaml at the repository root.
const DATABASE_URL =
  process.env.SPANLOG_PG_URL ??
  'postgres://spanlog:spanlog@127.0.0.1:5438/spanlog';

interface Order {
  id: number;
  sku: string;
}

const OrderEntity = new EntitySchema<Order>({
  name: 'Order',
  tableName: 'orders',
  columns: {
    id: { type: Number, primary: true },
    sku: { type: String },
  },
});

function transactionEnds(lines: Rendered[]): string[] {
  return lines
    .filter(
      (line) =>
        line.fields.phase === 'DONE' &&
        (line.fields.op === 'ROLLBACK' || line.fields.op === 'COMMIT'),
    )
    .map(shapeWithLevel);
}

function firstParameter(parameters: unknown): unknown {
  return Array.isArray(parameters) ? parameters[0] : undefined;
}

// A failing release or body still removes the subscriber, and later tests see
// only the subscriber of logQueries.
async function withSubscriberAndRunner(
  subscriber: EntitySubscriberInterface,
  body: (runner: QueryRunner) => Promise<void>,
): Promise<void> {
  dataSource.subscribers.push(subscriber);
  try {
    const runner = dataSource.createQueryRunner();
    try {
      await body(runner);
    } finally {
      await runner.release();
    }
  } finally {
    dataSource.subscribers.splice(
      dataSource.subscribers.indexOf(subscriber),
      1,
    );
  }
}

class OrderRepository {
  constructor(private readonly dataSource: DataSource) {}

  async create(id: number, sku: string): Promise<void> {
    await this.dataSource.getRepository(OrderEntity).save({ id, sku });
  }

  async insertRaw(id: number, sku: string): Promise<void> {
    await this.dataSource.query(
      'INSERT INTO orders (id, sku) VALUES ($1, $2)',
      [id, sku],
    );
  }

  async countBoth(): Promise<void> {
    await this.dataSource.transaction(async (manager) => {
      await Promise.all([
        manager.query('SELECT pg_sleep(0.05), COUNT(*) FROM orders'),
        manager.query('SELECT COUNT(*) FROM order_lines'),
      ]);
    });
  }

  async placeAndFail(id: number, sku: string): Promise<void> {
    await this.dataSource.transaction(async (manager) => {
      await manager.query('INSERT INTO orders (id, sku) VALUES ($1, $2)', [
        id,
        sku,
      ]);
      await manager.query(
        'INSERT INTO order_lines (order_id, sku) VALUES ($1, $2)',
        [id, sku],
      );
      await manager.query('SELECT COUNT(*) FROM orders');
      throw new Error('stock gone');
    });
  }

  async placeWithBadLine(id: number, sku: string): Promise<void> {
    await this.dataSource.transaction(async (manager) => {
      await manager.query('INSERT INTO orders (id, sku) VALUES ($1, $2)', [
        id,
        sku,
      ]);
      await manager.query(
        "INSERT INTO order_lines (order_id, sku) VALUES ('not a number', $1)",
        [sku],
      );
    });
  }
}

let dataSource: DataSource;
let written: Rendered[];
let log: Spanlog;
let orders: OrderRepository;
let detach: () => void;

beforeAll(async () => {
  dataSource = new DataSource({
    type: 'postgres',
    url: DATABASE_URL,
    entities: [OrderEntity],
    synchronize: true,
    dropSchema: true,
  });
  await dataSource.initialize();
  await dataSource.query(
    'CREATE TABLE IF NOT EXISTS order_lines (order_id int, sku text)',
  );
  written = [];
  log = createSpanlog({
    service: 'shop',
    level: 'debug',
    utcOffsetMinutes: 0,
    // A loaded machine must not turn an expected DONE into SLOW.
    slowMs: 60_000,
    sink: { write: (rendered) => written.push(rendered) },
  });
  orders = log.traced(new OrderRepository(dataSource), 'OrderRepository');
  detach = logQueries(dataSource, log);
});

afterAll(async () => {
  await dataSource?.destroy();
});

beforeEach(() => {
  written.length = 0;
});

describe('logQueries on Postgres', () => {
  it('writes the statements of save() in the frame that calls it', async () => {
    await log.withRequest('7f3a9c', () => orders.create(1, 'A1'));

    expect(written.map(shapeWithLevel)).toEqual([
      'info OrderRepository.create - run | START',
      'debug OrderRepository.create - query SELECT orders | START',
      'debug OrderRepository.create - query SELECT orders | DONE Nms',
      'debug OrderRepository.create - query START TRANSACTION | START',
      'debug OrderRepository.create - query START TRANSACTION | DONE Nms',
      'debug OrderRepository.create - query INSERT orders | START',
      'debug OrderRepository.create - query INSERT orders | DONE Nms',
      'debug OrderRepository.create - query COMMIT | START',
      'debug OrderRepository.create - query COMMIT | DONE Nms',
      'info OrderRepository.create - run | DONE Nms',
    ]);
    expect(written.every((line) => line.fields.reqId === '7f3a9c')).toBe(true);
  });

  it('writes a failed statement at warn and the error once on its frame', async () => {
    await orders.insertRaw(2, 'B2');
    written.length = 0;

    await expect(
      log.withRequest('b81e44', () => orders.insertRaw(2, 'B2')),
    ).rejects.toThrow(/duplicate key/);

    expect(written.map(shapeWithLevel)).toEqual([
      'info OrderRepository.insertRaw - run | START',
      'debug OrderRepository.insertRaw - query INSERT orders | START',
      'warn OrderRepository.insertRaw - query INSERT orders | FAILED Nms error: duplicate key value violates unique constraint "PK_710e2d4957aa5878dfe94e4ac2f"',
      'error OrderRepository.insertRaw - run | FAILED Nms QueryFailedError: duplicate key value violates unique constraint "PK_710e2d4957aa5878dfe94e4ac2f"',
    ]);
    expect(written[2]?.lines).toHaveLength(1);
    expect(written[3]?.lines.length).toBeGreaterThan(1);
  });

  it('matches each end line to its statement when one transaction runs two at once', async () => {
    await orders.countBoth();

    const queryLines = written
      .filter((line) => line.fields.action === 'query')
      .map(
        (line) => `${String(line.fields.target)} ${String(line.fields.phase)}`,
      );
    expect(queryLines).toEqual([
      'START TRANSACTION START',
      'START TRANSACTION DONE',
      'SELECT orders START',
      'SELECT order_lines START',
      'SELECT orders DONE',
      'SELECT order_lines DONE',
      'COMMIT START',
      'COMMIT DONE',
    ]);
    const slowOne = written.find(
      (line) =>
        line.fields.target === 'SELECT orders' && line.fields.phase === 'DONE',
    );
    expect(Number(slowOne?.fields.ms)).toBeGreaterThanOrEqual(45);
  });

  it('names the tables a failed transaction wrote on its ROLLBACK line', async () => {
    await expect(
      log.withRequest('c4d2e1', () => orders.placeAndFail(3, 'C3')),
    ).rejects.toThrow('stock gone');

    expect(written.map(shapeWithLevel)).toEqual([
      'info OrderRepository.placeAndFail - run | START',
      'debug OrderRepository.placeAndFail - query START TRANSACTION | START',
      'debug OrderRepository.placeAndFail - query START TRANSACTION | DONE Nms',
      'debug OrderRepository.placeAndFail - query INSERT orders | START',
      'debug OrderRepository.placeAndFail - query INSERT orders | DONE Nms',
      'debug OrderRepository.placeAndFail - query INSERT order_lines | START',
      'debug OrderRepository.placeAndFail - query INSERT order_lines | DONE Nms',
      'debug OrderRepository.placeAndFail - query SELECT orders | START',
      'debug OrderRepository.placeAndFail - query SELECT orders | DONE Nms',
      'debug OrderRepository.placeAndFail - query ROLLBACK orders, order_lines | START',
      'warn OrderRepository.placeAndFail - query ROLLBACK orders, order_lines | DONE Nms',
      'error OrderRepository.placeAndFail - run | FAILED Nms Error: stock gone',
    ]);
    expect(written[10]?.fields.tables).toEqual(['orders', 'order_lines']);
  });

  it('leaves a statement that failed off the ROLLBACK line', async () => {
    await expect(orders.placeWithBadLine(5, 'E5')).rejects.toThrow(
      /invalid input syntax/,
    );

    expect(transactionEnds(written)).toEqual([
      'warn OrderRepository.placeWithBadLine - query ROLLBACK orders | DONE Nms',
    ]);
  });

  it('names on ROLLBACK TO SAVEPOINT only the tables written since the savepoint', async () => {
    await dataSource.transaction(async (manager) => {
      await manager.query('INSERT INTO orders (id, sku) VALUES ($1, $2)', [
        6,
        'F6',
      ]);
      await manager
        .transaction(async (inner) => {
          await inner.query(
            'INSERT INTO order_lines (order_id, sku) VALUES ($1, $2)',
            [6, 'F6'],
          );
          throw new Error('line rejected');
        })
        .catch(() => undefined);
    });

    expect(transactionEnds(written)).toEqual([
      'warn query ROLLBACK order_lines | DONE Nms',
      'debug query COMMIT | DONE Nms',
    ]);
  });

  it('keeps the tables of two transactions that run at once apart', async () => {
    let openGate: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    const failing = dataSource.transaction(async (manager) => {
      await manager.query('INSERT INTO orders (id, sku) VALUES ($1, $2)', [
        7,
        'G7',
      ]);
      await gate;
      throw new Error('payment declined');
    });
    const passing = dataSource.transaction(async (manager) => {
      await manager.query(
        'INSERT INTO order_lines (order_id, sku) VALUES ($1, $2)',
        [7, 'G7'],
      );
      openGate();
    });
    await Promise.allSettled([failing, passing]);

    expect(transactionEnds(written).sort()).toEqual([
      'debug query COMMIT | DONE Nms',
      'warn query ROLLBACK orders | DONE Nms',
    ]);
  });

  it('matches each end line when another subscriber delays a statement', async () => {
    const delayOrders: EntitySubscriberInterface = {
      beforeQuery: async (event) => {
        if (event.query.includes('FROM orders')) {
          await new Promise((resolve) => setTimeout(resolve, 80));
        }
      },
    };
    await withSubscriberAndRunner(delayOrders, async (runner) => {
      await Promise.all([
        runner.query('SELECT COUNT(*) FROM orders'),
        runner.query('SELECT COUNT(*) FROM order_lines'),
      ]);
    });

    const delayed = written.find(
      (line) =>
        line.fields.target === 'SELECT orders' && line.fields.phase === 'DONE',
    );
    const direct = written.find(
      (line) =>
        line.fields.target === 'SELECT order_lines' &&
        line.fields.phase === 'DONE',
    );
    expect(Number(delayed?.fields.ms)).toBeGreaterThanOrEqual(75);
    expect(Number(direct?.fields.ms)).toBeLessThan(Number(delayed?.fields.ms));
  });

  it('keeps later statements paired when another subscriber stops one', async () => {
    const stopOrders: EntitySubscriberInterface = {
      beforeQuery: (event) => {
        if (event.query.includes('FROM orders')) {
          throw new Error('stopped');
        }
      },
    };
    await withSubscriberAndRunner(stopOrders, async (runner) => {
      await expect(runner.query('SELECT 1 FROM orders')).rejects.toThrow(
        'stopped',
      );
      await runner.query('SELECT 1 FROM order_lines');
    });

    expect(written.map(shapeWithLevel)).toEqual([
      'debug query SELECT orders | START',
      'debug query SELECT order_lines | START',
      'debug query SELECT order_lines | DONE Nms',
    ]);
  });

  it('writes a failure on the request whose statement failed when two share a text', async () => {
    const sql = 'SELECT 1 / $1::int';
    await withSubscriberAndRunner({}, async (runner) => {
      const failing = log.withRequest('failing', () => runner.query(sql, [0]));
      const working = log.withRequest('working', () => runner.query(sql, [1]));
      await expect(failing).rejects.toThrow('division by zero');
      await working;
    });

    expect(
      written.map((line) => `${line.fields.reqId} ${line.fields.phase}`),
    ).toEqual([
      'failing START',
      'working START',
      'failing FAILED',
      'working DONE',
    ]);
  });

  it('matches end lines when two statements share a text and one is delayed', async () => {
    const delayFirst: EntitySubscriberInterface = {
      beforeQuery: async (event) => {
        if (firstParameter(event.parameters) === 1) {
          await new Promise((resolve) => setTimeout(resolve, 80));
        }
      },
    };
    const sql = 'SELECT $1::int FROM orders';
    await withSubscriberAndRunner(delayFirst, async (runner) => {
      await Promise.all([
        log.withRequest('delayed', () => runner.query(sql, [1])),
        log.withRequest('direct', () => runner.query(sql, [2])),
      ]);
    });

    const doneOf = (reqId: string) =>
      written.find(
        (line) => line.fields.reqId === reqId && line.fields.phase === 'DONE',
      );
    const delayedMs = Number(doneOf('delayed')?.fields.ms);
    expect(delayedMs).toBeGreaterThanOrEqual(75);
    expect(Number(doneOf('direct')?.fields.ms)).toBeLessThan(delayedMs);
  });

  it('ends a statement on its own line after a stopped one with the same text', async () => {
    const sql = 'SELECT $1::int FROM orders';
    const stopFirst: EntitySubscriberInterface = {
      beforeQuery: (event) => {
        if (firstParameter(event.parameters) === 1) {
          throw new Error('stopped');
        }
      },
    };
    await withSubscriberAndRunner(stopFirst, async (runner) => {
      await expect(
        log.withRequest('stopped', () => runner.query(sql, [1])),
      ).rejects.toThrow('stopped');
      await log.withRequest('direct', () => runner.query(sql, [2]));
    });

    expect(
      written.map((line) => `${line.fields.reqId} ${line.fields.phase}`),
    ).toEqual(['stopped START', 'direct START', 'direct DONE']);
  });

  it.each([
    ['throws', false],
    ['rejects', true],
  ])(
    'ends a statement without parameters after a stopped one when a subscriber %s',
    async (_label, rejects) => {
      const sql = 'SELECT 1 FROM orders';
      let stopNext = true;
      const stopOnce: EntitySubscriberInterface = {
        beforeQuery: () => {
          if (!stopNext) {
            return undefined;
          }
          stopNext = false;
          if (rejects) {
            return Promise.reject(new Error('stopped'));
          }
          throw new Error('stopped');
        },
      };
      await withSubscriberAndRunner(stopOnce, async (runner) => {
        await expect(
          log.withRequest('stopped', () => runner.query(sql)),
        ).rejects.toThrow('stopped');
        await log.withRequest('direct', () => runner.query(sql));
      });

      expect(
        written.map((line) => `${line.fields.reqId} ${line.fields.phase}`),
      ).toEqual(['stopped START', 'direct START', 'direct DONE']);
    },
  );

  it('keeps a parameter value out of the lines with errorMessage', async () => {
    const hiddenLines: Rendered[] = [];
    const hiddenLog = createSpanlog({
      service: 'shop',
      level: 'debug',
      errorMessage: (error) => `${error.name} hidden`,
      sink: { write: (rendered) => hiddenLines.push(rendered) },
    });
    const detachHidden = logQueries(dataSource, hiddenLog);
    const lookup = hiddenLog.traced(
      {
        byId: async (id: string) =>
          dataSource.query('SELECT * FROM orders WHERE id = $1', [id]),
      },
      'Lookup',
    );

    try {
      await expect(lookup.byId('private@example.test')).rejects.toThrow(
        /private@example\.test/,
      );
    } finally {
      detachHidden();
    }

    expect(hiddenLines.at(-1)?.fields.err).toHaveProperty('stack');
    expect(hiddenLines.at(-1)?.lines.length).toBeGreaterThan(1);
    expect(JSON.stringify(hiddenLines)).not.toContain('private@example.test');
  });

  it('stops writing after the returned function runs', async () => {
    detach();
    try {
      await dataSource.query('SELECT 1');

      expect(written).toEqual([]);
    } finally {
      detach = logQueries(dataSource, log);
    }
  });

  it('frees its store when the returned function runs after destroy and initialize', async () => {
    const restarted = new DataSource({ type: 'postgres', url: DATABASE_URL });
    await restarted.initialize();
    const disable = vi.spyOn(AsyncLocalStorage.prototype, 'disable');

    try {
      const detachRestarted = logQueries(restarted, log);
      await restarted.destroy();
      await restarted.initialize();
      const current = restarted.subscribers;
      detachRestarted();
      detachRestarted();

      expect(disable).toHaveBeenCalledTimes(1);
      expect(restarted.subscribers).toBe(current);
    } finally {
      disable.mockRestore();
      // destroy on a DataSource that failed to initialize would replace that error.
      if (restarted.isInitialized) {
        await restarted.destroy();
      }
    }
  });

  it('refuses a DataSource that is not initialized', () => {
    const idle = new DataSource({ type: 'postgres', url: DATABASE_URL });

    expect(() => logQueries(idle, log)).toThrow(
      'Call it after `await dataSource.initialize()`',
    );
  });
});
