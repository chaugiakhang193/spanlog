import { describe, expect, it } from 'vitest';
import { createSpanlog, type SpanlogOptions } from './spanlog.js';
import type { Rendered } from './types.js';

const MS = /\d+(\.\d+)?ms/;

function setup(options: Partial<SpanlogOptions> = {}) {
  const written: Rendered[] = [];
  const spanlog = createSpanlog({
    service: 'shop',
    sink: { write: (rendered) => written.push(rendered) },
    utcOffsetMinutes: 0,
    level: 'debug',
    ...options,
  });
  return { spanlog, written };
}

// shape hides the measured duration, which changes from run to run.
function shape(rendered: Rendered): string {
  return `${rendered.level} ${rendered.msg.replace(MS, 'Nms')}`;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

describe('startRequest', () => {
  it('writes START and DONE with the status and the reqId', () => {
    const { spanlog, written } = setup();
    spanlog.withRequest('7f3a9c', () => {
      spanlog.startRequest('POST /orders').end(201);
    });

    expect(written.map(shape)).toEqual([
      'info POST /orders | START',
      'info POST /orders | DONE 201 Nms',
    ]);
    expect(written[1]?.fields).toMatchObject({
      reqId: '7f3a9c',
      action: 'request',
      target: 'POST /orders',
      phase: 'DONE',
      status: 201,
    });
  });

  it('ends with FAILED at warn for a status of 500 or higher', () => {
    const { spanlog, written } = setup();
    spanlog.withRequest('b81e44', () => {
      spanlog.startRequest('POST /orders').end(500);
    });

    expect(written.map(shape)).toEqual([
      'info POST /orders | START',
      'warn POST /orders | FAILED 500 Nms',
    ]);
  });

  it('keeps a 4xx status as DONE', () => {
    const { spanlog, written } = setup();
    spanlog.startRequest('GET /orders/9').end(404);

    expect(shape(written[1]!)).toBe('info GET /orders/9 | DONE 404 Nms');
  });

  it('ends with SLOW after slowMs', async () => {
    const { spanlog, written } = setup({ slowMs: 5 });
    const span = spanlog.startRequest('GET /report');
    await delay(15);
    span.end(200);

    expect(shape(written[1]!)).toBe('warn GET /report | SLOW 200 Nms');
  });

  it('writes ABORTED without a status', () => {
    const { spanlog, written } = setup();
    spanlog.startRequest('GET /stream').abort();

    expect(shape(written[1]!)).toBe('warn GET /stream | ABORTED Nms');
    expect(written[1]?.fields).not.toHaveProperty('status');
  });

  it('writes only the first end line', () => {
    const { spanlog, written } = setup();
    const span = spanlog.startRequest('GET /orders');
    span.end(200);
    span.end(500);
    span.abort();

    expect(written.map(shape)).toEqual([
      'info GET /orders | START',
      'info GET /orders | DONE 200 Nms',
    ]);
  });

  it('keeps the reqId of its request when it ends outside the request', () => {
    const { spanlog, written } = setup();
    const span = spanlog.withRequest('7f3a9c', () =>
      spanlog.startRequest('GET /orders'),
    );
    span.end(200);

    expect(written[1]?.fields.reqId).toBe('7f3a9c');
  });
});

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
    const { spanlog, written } = setup();
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

    expect(written.map(shape)).toEqual([
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
    const { spanlog, written } = setup();
    spanlog.startQuery("SELECT * FROM users WHERE email = 'a@b.c'").end();

    const everything = JSON.stringify(written);
    expect(everything).not.toContain('a@b.c');
    expect(everything).not.toContain('WHERE');
  });

  it('keeps a string literal out of the tables', () => {
    const { spanlog, written } = setup();
    spanlog
      .startQuery("INSERT INTO notes (body) VALUES ('sent from alice_secret')")
      .end();

    expect(written[0]?.fields.tables).toEqual(['notes']);
    expect(JSON.stringify(written)).not.toContain('alice_secret');
  });

  it('never writes the statement text when the statement fails', () => {
    const { spanlog, written } = setup();
    spanlog
      .startQuery("SELECT * FROM users WHERE email = 'a@b.c'")
      .fail(new Error('boom'));

    const everything = JSON.stringify(written);
    expect(everything).not.toContain('a@b.c');
    expect(everything).not.toContain('WHERE');
  });

  it('writes no frame outside a wrapped method', () => {
    const { spanlog, written } = setup();
    spanlog.startQuery('COMMIT').end();

    expect(written.map(shape)).toEqual([
      'debug query COMMIT | START',
      'debug query COMMIT | DONE Nms',
    ]);
    expect(written[0]?.lines[0]).toContain('req=-');
  });

  it('ends a ROLLBACK at warn', () => {
    const { spanlog, written } = setup({ level: 'info' });
    spanlog.startQuery('ROLLBACK').end();

    expect(written.map(shape)).toEqual(['warn query ROLLBACK | DONE Nms']);
  });

  it('names the tables an adapter gives a ROLLBACK', () => {
    const { spanlog, written } = setup();
    spanlog.startQuery('ROLLBACK', ['orders', 'warehouse.inventory']).end();

    expect(written.map(shape)).toEqual([
      'debug query ROLLBACK orders, warehouse.inventory | START',
      'warn query ROLLBACK orders, warehouse.inventory | DONE Nms',
    ]);
    expect(written[1]?.fields.tables).toEqual([
      'orders',
      'warehouse.inventory',
    ]);
  });

  it('keeps the tables of its text for a statement other than ROLLBACK', () => {
    const { spanlog, written } = setup();
    spanlog.startQuery('COMMIT', ['orders']).end();

    expect(written.map(shape)).toEqual([
      'debug query COMMIT | START',
      'debug query COMMIT | DONE Nms',
    ]);
  });

  it('fails at warn without a stack and leaves the error to its frame', async () => {
    const { spanlog, written } = setup();
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

    expect(written.map(shape)).toEqual([
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
    const { spanlog, written } = setup();
    const span = spanlog.startQuery('COMMIT');
    span.end();
    span.fail(new Error('late'));

    expect(written).toHaveLength(2);
  });

  it('ends with SLOW after slowMs', async () => {
    const { spanlog, written } = setup({ slowMs: 5 });
    const span = spanlog.startQuery('SELECT * FROM orders');
    await delay(15);
    span.end();

    expect(shape(written[1]!)).toBe('warn query SELECT orders | SLOW Nms');
  });
});

describe('errorMessage option', () => {
  class Repository {
    constructor(private readonly failure: unknown) {}

    async load(): Promise<void> {
      throw this.failure;
    }
  }

  const SECRET = 'private@example.test';

  it('writes the message of an Error as it is by default', async () => {
    const { spanlog, written } = setup();
    const repository = spanlog.traced(
      new Repository(new Error(`bad value ${SECRET}`)),
      'Repository',
    );

    await expect(repository.load()).rejects.toThrow(SECRET);

    expect(written.at(-1)?.msg).toContain(SECRET);
  });

  it('replaces the message on every line and in the stack', async () => {
    const { spanlog, written } = setup({
      errorMessage: (error) => `${error.name} hidden`,
    });
    const failure = new Error(
      `bad value ${SECRET}\n    at ${SECRET} (looks.js:1:1)`,
    );
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await expect(repository.load()).rejects.toBe(failure);

    const line = written.at(-1);
    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(line?.msg).toContain('Error: Error hidden');
    expect(line?.fields.err).toMatchObject({ message: 'Error hidden' });
    expect(String((line?.fields.err as { stack: string }).stack)).toMatch(
      /^Error: Error hidden\n\s+at /,
    );
    expect(line?.lines.length).toBeGreaterThan(1);
  });

  it('keeps the frames when the name changed after the stack was made', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const failure = new Error(`bad value ${SECRET}`);
    expect(failure.stack).toContain('Error: bad value');
    failure.name = 'CustomError';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.fields.err).toMatchObject({ name: 'CustomError' });
    expect(written.at(-1)?.lines.length).toBeGreaterThan(1);
  });

  it('keeps the frames when the name holds a colon and a space', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const failure = new Error(`bad value ${SECRET}`);
    failure.name = 'Outer: Inner';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    const err = written.at(-1)?.fields.err as { stack: string };
    expect(err.stack).toMatch(/^Outer: Inner: hidden\n\s+at /);
  });

  // V8 writes a first line without the separator when the name or the message
  // is empty. The source map handler of the test runner writes the separator.
  // These stacks are set by hand to hold the first line of V8.
  it.each([
    [
      'holds a colon and the message is empty',
      'Outer: Inner',
      '',
      'Outer: Inner',
    ],
    ['is empty', '', `bad value ${SECRET}`, `bad value ${SECRET}`],
    ['and the message are empty', '', '', ''],
  ])(
    'keeps the frames when the name %s',
    async (_label, name, message, firstLine) => {
      const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
      const failure = new Error(message);
      failure.name = name;
      failure.stack = `${firstLine}\n    at a (a.js:1:1)`;
      const repository = spanlog.traced(new Repository(failure), 'Repository');

      await repository.load().catch(() => undefined);

      expect(JSON.stringify(written)).not.toContain(SECRET);
      const err = written.at(-1)?.fields.err as { stack: string };
      expect(err.stack).toBe(`${name}: hidden\n    at a (a.js:1:1)`);
    },
  );

  it.each([
    ['the name is empty', '', 'old header'],
    ['the name was assigned', 'CustomError', 'old_header'],
  ])(
    'keeps no frames of a first line that holds neither name nor message when %s',
    async (_label, name, firstLine) => {
      const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
      const failure = new Error('');
      failure.name = name;
      failure.stack = `${firstLine}\n    at ${SECRET} (old.js:1:1)`;
      const repository = spanlog.traced(new Repository(failure), 'Repository');

      await repository.load().catch(() => undefined);

      expect(JSON.stringify(written)).not.toContain(SECRET);
      const err = written.at(-1)?.fields.err as { stack: string };
      expect(err.stack).toBe(`${name}: hidden`);
    },
  );

  it('drops the old lines when the message changed to its first line', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const failure = new Error(`bad value\ndetail ${SECRET}`);
    expect(failure.stack).toContain(SECRET);
    failure.message = 'bad value';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.lines.length).toBeGreaterThan(1);
  });

  it('keeps the frames of an Error without a message', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const repository = spanlog.traced(
      new Repository(new Error('')),
      'Repository',
    );

    await repository.load().catch(() => undefined);

    expect(written.at(-1)?.lines.length).toBeGreaterThan(1);
  });

  it('reads a stack whose first line is the name alone', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const failure = new Error('');
    failure.stack = 'Error\n    at a (a.js:1:1)';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    const err = written.at(-1)?.fields.err as { stack: string };
    expect(err.stack).toBe('Error: hidden\n    at a (a.js:1:1)');
  });

  it('keeps a frame that holds the text of the message', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const failure = new Error('load');
    failure.stack = 'Error: load\n    at load (a.js:1:1)\n    at b (a.js:2:2)';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    const err = written.at(-1)?.fields.err as { stack: string };
    expect(err.stack).toBe(
      'Error: hidden\n    at load (a.js:1:1)\n    at b (a.js:2:2)',
    );
  });

  it('keeps the frames when the message changed before the stack was read', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const failure = new Error(`bad value ${SECRET}`);
    failure.message = 'wrapped';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    expect(written.at(-1)?.lines.length).toBeGreaterThan(1);
  });

  // Node 24 writes the stack text at the first read of err.stack. A message
  // assigned after that read leaves the old message in the stack.
  describe.each([
    ['another message', 'wrapped'],
    ['a message of the same length', 'ten chars!'],
    ['an empty message', ''],
    ['the tail of the old first line', 'value'],
  ])('when the message changed to %s', (_label, changedMessage) => {
    it('keeps nothing of the old message', async () => {
      const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
      const failure = new Error(`bad: value\n    at ${SECRET} (looks.js:1:1)`);
      expect(failure.stack).toContain(SECRET);
      failure.message = changedMessage;
      const repository = spanlog.traced(new Repository(failure), 'Repository');

      await repository.load().catch(() => undefined);

      expect(JSON.stringify(written)).not.toContain(SECRET);
      expect(written.at(-1)?.fields.err).toMatchObject({
        message: 'hidden',
        stack: 'Error: hidden',
      });
    });
  });

  it('replaces the message of a failed statement', () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    spanlog
      .startQuery('SELECT 1 FROM orders')
      .fail(new Error(`bad value ${SECRET}`));

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.fields.err).toEqual({
      name: 'Error',
      message: 'hidden',
    });
  });

  it('replaces the message of a thrown string, which becomes an Error', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const repository = spanlog.traced(new Repository(SECRET), 'Repository');

    await expect(repository.load()).rejects.toThrow(SECRET);

    expect(JSON.stringify(written)).not.toContain(SECRET);
  });

  it('hides the message when errorMessage throws', async () => {
    const { spanlog, written } = setup({
      errorMessage: () => {
        throw new Error('broken');
      },
    });
    const failure = new Error(`bad value ${SECRET}`);
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await expect(repository.load()).rejects.toBe(failure);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.fields.phase).toBe('FAILED');
    expect(written.at(-1)?.fields.err).toMatchObject({
      name: 'UnreadableError',
    });
  });

  it('leaves a thrown plain object to redact', async () => {
    const { spanlog, written } = setup({ errorMessage: () => 'hidden' });
    const repository = spanlog.traced(
      new Repository({ code: 'E1', password: SECRET }),
      'Repository',
    );

    await repository.load().catch(() => undefined);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.msg).toContain('E1');
  });
});
