import { AsyncLocalStorage } from 'node:async_hooks';
import type {
  AfterQueryEvent,
  BeforeQueryEvent,
  DataSource,
  EntitySubscriberInterface,
  QueryRunner,
} from 'typeorm';
import { PostgresQueryRunner } from 'typeorm/driver/postgres/PostgresQueryRunner.js';
import { SqljsQueryRunner } from 'typeorm/driver/sqljs/SqljsQueryRunner.js';
import { Broadcaster } from 'typeorm/subscriber/Broadcaster.js';
import { describe, expect, it, vi } from 'vitest';
import { createSpanlog } from './spanlog.js';
import { logQueries } from './typeorm.js';
import type { Rendered } from './types.js';

const SQL = 'SELECT 1 FROM orders';
const QUIET_LOGGER = {
  logQuery: () => undefined,
  logQueryError: () => undefined,
  logQuerySlow: () => undefined,
};

interface Held {
  resolve: () => void;
  reject: (failure: Error) => void;
}

function setup() {
  const lines: Rendered[] = [];
  const log = createSpanlog({
    service: 'shop',
    level: 'debug',
    slowMs: 60_000,
    sink: { write: (rendered) => lines.push(rendered) },
  });
  const subscribers: EntitySubscriberInterface[] = [];
  const dataSource = {
    isInitialized: true,
    subscribers,
    logger: QUIET_LOGGER,
  };
  logQueries(dataSource as unknown as DataSource, log);
  const written = () =>
    lines.map(
      (line) => `${String(line.fields.reqId)} ${String(line.fields.phase)}`,
    );
  return { log, lines, written, subscribers, dataSource };
}

function withBroadcaster<T extends QueryRunner>(runner: T): T {
  Object.assign(runner, { broadcaster: new Broadcaster(runner) });
  return runner;
}

// postgresRunner runs the query method of TypeORM on a connection that holds
// each statement until the test ends it. reached resolves with the statements
// held so far once count of them arrived.
function postgresRunner(dataSource: unknown) {
  const held: Held[] = [];
  const waiting: { count: number; resolve: () => void }[] = [];
  const connection = {
    query: () =>
      new Promise((resolve, reject) => {
        held.push({
          resolve: () => resolve({ rows: [], rowCount: 0, command: 'SELECT' }),
          reject,
        });
        waiting
          .filter((wait) => wait.count === held.length)
          .forEach((wait) => wait.resolve());
      }),
  };
  const runner = Object.create(
    PostgresQueryRunner.prototype,
  ) as PostgresQueryRunner;
  Object.assign(runner, {
    isReleased: false,
    dataSource,
    driver: { dataSource, options: {} },
    connect: () => Promise.resolve(connection),
  });
  const reached = (count: number): Promise<void> =>
    held.length >= count
      ? Promise.resolve()
      : new Promise((resolve) => waiting.push({ count, resolve }));
  return { runner: withBroadcaster(runner), held, reached };
}

// The query method of the SQL.js driver maps parameters to a new array before
// afterQuery. A statement bound to 1 fails on this connection.
function sqljsRunner(dataSource: unknown): SqljsQueryRunner {
  const databaseConnection = {
    prepare: () => {
      let bound: unknown[] = [];
      return {
        bind: (parameters: unknown[]) => {
          bound = parameters;
        },
        step: () => {
          if (bound[0] === 1) {
            throw new Error('bound to one');
          }
          return false;
        },
        getAsObject: () => ({}),
        free: () => undefined,
      };
    },
    getRowsModified: () => 0,
  };
  const runner = Object.create(SqljsQueryRunner.prototype) as SqljsQueryRunner;
  Object.assign(runner, {
    isReleased: false,
    dataSource,
    driver: { dataSource, options: {}, databaseConnection },
  });
  return withBroadcaster(runner);
}

describe('logQueries pairing', () => {
  it('ends each statement on its own request when they end in start order', async () => {
    const { log, written, dataSource } = setup();
    const { runner, held, reached } = postgresRunner(dataSource);

    const first = log.withRequest('first', () => runner.query(SQL));
    const second = log.withRequest('second', () => runner.query(SQL));
    await reached(2);
    held[0]?.reject(new Error('first failed'));
    await expect(first).rejects.toThrow('first failed');
    held[1]?.resolve();
    await second;

    expect(written()).toEqual([
      'first START',
      'second START',
      'first FAILED',
      'second DONE',
    ]);
  });

  it('ends each statement on its own request when they end in reverse order', async () => {
    const { log, written, dataSource } = setup();
    const { runner, held, reached } = postgresRunner(dataSource);

    const first = log.withRequest('first', () => runner.query(SQL));
    const second = log.withRequest('second', () => runner.query(SQL));
    await reached(2);
    held[1]?.reject(new Error('second failed'));
    await expect(second).rejects.toThrow('second failed');
    held[0]?.resolve();
    await first;

    expect(written()).toEqual([
      'first START',
      'second START',
      'second FAILED',
      'first DONE',
    ]);
  });

  it('measures each statement from its own start within one request', async () => {
    const { log, lines, dataSource } = setup();
    const { runner, held, reached } = postgresRunner(dataSource);
    let clock = 1000;
    const now = vi.spyOn(performance, 'now').mockImplementation(() => clock);

    try {
      await log.withRequest('one', async () => {
        const early = runner.query(SQL);
        await reached(1);
        clock += 60;
        const late = runner.query(SQL);
        await reached(2);
        clock += 5;
        held[0]?.resolve();
        await early;
        clock += 2;
        held[1]?.resolve();
        await late;
      });
    } finally {
      now.mockRestore();
    }

    const durations = lines
      .filter((line) => line.fields.phase === 'DONE')
      .map((line) => Number(line.fields.ms));
    expect(durations).toEqual([65, 7]);
  });

  it('ends each statement when the driver maps parameters to a new array', async () => {
    const { log, written, dataSource } = setup();
    const runner = sqljsRunner(dataSource);

    await Promise.allSettled([
      log.withRequest('first', () => runner.query(SQL, [1])),
      log.withRequest('second', () => runner.query(SQL, [2])),
    ]);

    expect(written()).toEqual([
      'first START',
      'second START',
      'first FAILED',
      'second DONE',
    ]);
  });

  it('ends a statement on its own request when another subscriber delays the first', async () => {
    const { log, written, subscribers, dataSource } = setup();
    const { runner, held, reached } = postgresRunner(dataSource);
    let releaseFirst: () => void = () => undefined;
    let seen = 0;
    subscribers.push({
      beforeQuery: () => {
        seen += 1;
        return seen === 1
          ? new Promise<void>((resolve) => {
              releaseFirst = resolve;
            })
          : undefined;
      },
    });

    const first = log.withRequest('first', () => runner.query(SQL));
    const second = log.withRequest('second', () => runner.query(SQL));
    await reached(1);
    held[0]?.reject(new Error('second failed'));
    await expect(second).rejects.toThrow('second failed');
    releaseFirst();
    await reached(2);
    held[1]?.resolve();
    await first;

    expect(written()).toEqual([
      'first START',
      'second START',
      'second FAILED',
      'first DONE',
    ]);
  });

  it('ends the later statement after another subscriber stops one', async () => {
    const { log, written, subscribers, dataSource } = setup();
    const { runner, held, reached } = postgresRunner(dataSource);
    let stop = true;
    subscribers.push({
      beforeQuery: () => {
        if (stop) {
          stop = false;
          throw new Error('stopped');
        }
      },
    });

    await expect(
      log.withRequest('stopped', () => runner.query(SQL)),
    ).rejects.toThrow('stopped');
    const direct = log.withRequest('direct', () => runner.query(SQL));
    await reached(1);
    held[0]?.resolve();
    await direct;

    expect(written()).toEqual(['stopped START', 'direct START', 'direct DONE']);
  });

  it('writes one line pair per statement run one after another', async () => {
    const { log, written, dataSource } = setup();
    const runner = sqljsRunner(dataSource);

    await log.withRequest('one', async () => {
      await runner.query(SQL, [2]);
      await runner.query(SQL, [2]);
    });

    expect(written()).toEqual([
      'one START',
      'one DONE',
      'one START',
      'one DONE',
    ]);
  });

  // The query method of the SQL.js driver reaches beforeQuery without an await.
  // A statement issued from a query event there replaces the store of the
  // statement around it.
  describe('statement issued from a query event', () => {
    const OUTER = 'SELECT 1 FROM outer_table';
    const INNER = 'SELECT 1 FROM inner_table';
    const targets = (lines: Rendered[]) =>
      lines.map(
        (line) => `${String(line.fields.target)} ${String(line.fields.phase)}`,
      );

    it('leaves the result of the outer statement off the inner one from afterQuery', async () => {
      const { lines, subscribers, dataSource } = setup();
      const runner = sqljsRunner(dataSource);
      let inner: Promise<unknown> | undefined;
      subscribers.unshift({
        afterQuery: (event: AfterQueryEvent) => {
          if (event.query === OUTER) {
            inner = runner.query(INNER, [2]);
          }
        },
      });

      await expect(runner.query(OUTER, [1])).rejects.toThrow('bound to one');
      await inner;

      expect(targets(lines)).toEqual([
        'SELECT outer_table START',
        'SELECT inner_table START',
        'SELECT inner_table DONE',
      ]);
    });

    it('leaves the result of the outer statement off the inner one from beforeQuery', async () => {
      const { lines, subscribers, dataSource } = setup();
      const runner = sqljsRunner(dataSource);
      let inner: Promise<unknown> | undefined;
      let releaseInner: () => void = () => undefined;
      subscribers.push({
        beforeQuery: (event: BeforeQueryEvent) => {
          if (event.query === OUTER) {
            inner = runner.query(INNER, [2]);
            return undefined;
          }
          return new Promise<void>((resolve) => {
            releaseInner = resolve;
          });
        },
      });

      await expect(runner.query(OUTER, [1])).rejects.toThrow('bound to one');
      releaseInner();
      await inner;

      expect(targets(lines)).toEqual([
        'SELECT outer_table START',
        'SELECT inner_table START',
        'SELECT inner_table DONE',
      ]);
    });
  });

  it('frees its store when the subscriber is removed', () => {
    const lines: Rendered[] = [];
    const log = createSpanlog({
      service: 'shop',
      sink: { write: (rendered) => lines.push(rendered) },
    });
    const disable = vi.spyOn(AsyncLocalStorage.prototype, 'disable');
    const dataSource = { isInitialized: true, subscribers: [] };

    try {
      const detach = logQueries(dataSource as unknown as DataSource, log);
      detach();
      detach();

      expect(dataSource.subscribers).toEqual([]);
      expect(disable).toHaveBeenCalledTimes(1);
    } finally {
      disable.mockRestore();
    }
  });

  it('leaves its store disabled when the sink detaches during the START line', async () => {
    const lines: Rendered[] = [];
    let detach: () => void = () => undefined;
    const log = createSpanlog({
      service: 'shop',
      level: 'debug',
      slowMs: 60_000,
      sink: {
        write: (rendered) => {
          lines.push(rendered);
          detach();
        },
      },
    });
    const dataSource = {
      isInitialized: true,
      subscribers: [],
      logger: QUIET_LOGGER,
    };
    const { runner, held, reached } = postgresRunner(dataSource);
    const disable = vi.spyOn(AsyncLocalStorage.prototype, 'disable');
    const enterWith = vi.spyOn(AsyncLocalStorage.prototype, 'enterWith');

    try {
      detach = logQueries(dataSource as unknown as DataSource, log);
      const query = runner.query(SQL);
      await reached(1);
      held[0]?.resolve();
      await query;

      expect(lines.map((line) => line.fields.phase)).toEqual(['START']);
      expect(disable).toHaveBeenCalledTimes(1);
      expect(enterWith.mock.contexts).not.toContain(disable.mock.contexts[0]);
    } finally {
      disable.mockRestore();
      enterWith.mockRestore();
    }
  });

  describe('detach during a query event', () => {
    function setupDetachOn(phase: string) {
      const lines: Rendered[] = [];
      let detach: () => void = () => undefined;
      const log = createSpanlog({
        service: 'shop',
        level: 'debug',
        slowMs: 60_000,
        sink: {
          write: (rendered) => {
            lines.push(rendered);
            if (rendered.fields.phase === phase) {
              detach();
            }
          },
        },
      });
      const dataSource = {
        isInitialized: true,
        subscribers: [] as EntitySubscriberInterface[],
        logger: QUIET_LOGGER,
      };
      detach = logQueries(dataSource as unknown as DataSource, log);
      return { lines, dataSource, detach: () => detach() };
    }

    it.each(['START', 'DONE'])(
      'keeps the events of the next subscriber when the sink detaches on %s',
      async (phase) => {
        const { dataSource } = setupDetachOn(phase);
        const seen: string[] = [];
        dataSource.subscribers.push({
          beforeQuery: () => {
            seen.push('before');
          },
          afterQuery: () => {
            seen.push('after');
          },
        });
        const { runner, held, reached } = postgresRunner(dataSource);

        const query = runner.query(SQL);
        await reached(1);
        held[0]?.resolve();
        await query;

        expect(seen).toEqual(['before', 'after']);
      },
    );

    it('writes no line when a subscriber before it detaches in beforeQuery', async () => {
      const { lines, dataSource, detach } = setupDetachOn('none');
      dataSource.subscribers.unshift({ beforeQuery: () => detach() });
      const { runner, held, reached } = postgresRunner(dataSource);

      const query = runner.query(SQL);
      await reached(1);
      held[0]?.resolve();
      await query;

      expect(lines).toEqual([]);
    });

    it('writes no end line when a subscriber before it detaches in afterQuery', async () => {
      const { lines, dataSource, detach } = setupDetachOn('none');
      dataSource.subscribers.unshift({ afterQuery: () => detach() });
      const { runner, held, reached } = postgresRunner(dataSource);

      const query = runner.query(SQL);
      await reached(1);
      held[0]?.resolve();
      await query;

      expect(lines.map((line) => line.fields.phase)).toEqual(['START']);
    });

    // With AsyncContextFrame, disable() only clears the store of the context that
    // calls it. The context of the statement keeps its store.
    it('writes no end line when the detach runs in another async context', async () => {
      const { lines, dataSource, detach } = setupDetachOn('none');
      const outsideTheStatement = AsyncLocalStorage.snapshot();
      dataSource.subscribers.unshift({
        afterQuery: () => outsideTheStatement(detach),
      });
      const { runner, held, reached } = postgresRunner(dataSource);

      const query = runner.query(SQL);
      await reached(1);
      held[0]?.resolve();
      await query;

      expect(lines.map((line) => line.fields.phase)).toEqual(['START']);
    });
  });

  it('frees its store when initialize replaced the subscribers before the detach', () => {
    const log = createSpanlog({
      service: 'shop',
      sink: { write: () => undefined },
    });
    const disable = vi.spyOn(AsyncLocalStorage.prototype, 'disable');
    const dataSource = { isInitialized: true, subscribers: [] };

    try {
      const detach = logQueries(dataSource as unknown as DataSource, log);
      const replaced: EntitySubscriberInterface[] = [];
      Object.assign(dataSource, { subscribers: replaced });
      detach();
      detach();

      expect(disable).toHaveBeenCalledTimes(1);
      expect(dataSource.subscribers).toBe(replaced);
    } finally {
      disable.mockRestore();
    }
  });

  it('ignores an end without a matching statement', () => {
    const { written, subscribers } = setup();

    void subscribers[0]?.afterQuery?.({
      queryRunner: {},
      query: SQL,
      success: true,
    } as unknown as AfterQueryEvent);

    expect(written()).toEqual([]);
  });
});
