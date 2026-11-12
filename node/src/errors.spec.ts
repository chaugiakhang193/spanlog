import { runInNewContext } from 'node:vm';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Spanlog } from './spanlog.js';
import { delay, setup, setupAtDebug, shape } from './testing/helpers.js';
import type { ErrorInfo, Rendered } from './types.js';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('errors', () => {
  function wireChain(spanlog: Spanlog, failure: unknown) {
    class Inner {
      async fail(): Promise<void> {
        throw failure;
      }
    }
    class Middle {
      inner!: Inner;
      async step(): Promise<void> {
        await this.inner.fail();
      }
    }
    class Outer {
      middle!: Middle;
      async run(): Promise<void> {
        await this.middle.step();
      }
    }
    const outer = spanlog.traced(new Outer(), 'Outer');
    outer.middle = spanlog.traced(new Middle(), 'Middle');
    outer.middle.inner = spanlog.traced(new Inner(), 'Inner');
    return outer;
  }

  it('reports an error with its stack once and rethrows the same object', async () => {
    const boom = new Error('stock not available');
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, boom);

    await expect(spanlog.withRequest('r-5', () => outer.run())).rejects.toBe(
      boom,
    );

    const failed = written.filter((l) => l.fields.phase === 'FAILED');
    expect(failed.map((l) => `${l.level} ${shape(l)}`)).toEqual([
      'error Middle.step - call Inner.fail | FAILED Nms Error: stock not available',
      'info Outer.run - call Middle.step | FAILED Nms Error: stock not available',
      'info Outer.run - run | FAILED Nms Error: stock not available',
    ]);
    expect(failed[0]?.fields.err).toMatchObject({
      name: 'Error',
      message: 'stock not available',
      stack: boom.stack,
    });
    expect(failed[0]?.lines.length).toBeGreaterThan(1);
    expect(failed[1]?.fields.err).toEqual({
      name: 'Error',
      message: 'stock not available',
    });
    expect(failed[2]?.lines).toHaveLength(1);
  });

  it('reports the same error object once in each request', async () => {
    const shared = new Error('shared');
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, shared);

    await Promise.allSettled([
      spanlog.withRequest('a', () => outer.run()),
      spanlog.withRequest('b', () => outer.run()),
    ]);

    const errors = written.filter((l) => l.level === 'error');
    expect(errors.map((l) => l.fields.reqId).sort()).toEqual(['a', 'b']);
  });

  it('wraps a thrown string in an Error and rethrows the wrapper', async () => {
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, 'plain text');

    const outcome = spanlog.withRequest('r-6', () => outer.run());

    await expect(outcome).rejects.toBeInstanceOf(Error);
    await expect(outcome).rejects.toMatchObject({
      message: 'plain text',
      cause: 'plain text',
    });
    expect(written.filter((l) => l.level === 'error')).toHaveLength(1);
  });

  it('describes a thrown plain object by its redacted JSON', async () => {
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, { code: 'E1', token: 't' });

    await expect(outer.run()).rejects.toEqual({ code: 'E1', token: 't' });

    expect(written.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'ThrownValue',
      message: '{"code":"E1","token":"[REDACTED]"}',
    });
  });

  it('rethrows a thrown object whose getter throws while it is described', async () => {
    const thrown = {};
    Object.defineProperty(thrown, 'detail', {
      enumerable: true,
      get() {
        throw new Error('getter failed');
      },
    });
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, thrown);

    await expect(outer.run()).rejects.toBe(thrown);

    expect(written.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'UnreadableError',
      message: '[unreadable thrown value]',
    });
  });

  it('describes a thrown function by its name', async () => {
    function refused(): void {}
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, refused);

    await expect(outer.run()).rejects.toBe(refused);

    expect(written.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'ThrownValue',
      message: '[Function refused]',
    });
  });

  it('marks an Error whose message getter throws as unreadable', async () => {
    const thrown = new Error('x');
    Object.defineProperty(thrown, 'message', {
      get() {
        throw new Error('getter failed');
      },
    });
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, thrown);

    await expect(outer.run()).rejects.toBe(thrown);

    expect(written.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'UnreadableError',
      message: '[unreadable thrown value]',
    });
  });

  it('describes an Error from another realm by its name, message and stack', async () => {
    const foreign = runInNewContext('new RangeError("realm failure")') as {
      stack: string;
    };
    const { spanlog, written } = setup();
    const outer = wireChain(spanlog, foreign);

    await expect(outer.run()).rejects.toBe(foreign);

    expect(written.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'RangeError',
      message: 'realm failure',
      stack: foreign.stack,
    });
  });

  it('keeps counting reports when the level hides the outer lines', async () => {
    const { spanlog, written } = setup({ level: 'error' });
    const outer = wireChain(spanlog, new Error('x'));

    await expect(spanlog.withRequest('r-7', () => outer.run())).rejects.toThrow(
      'x',
    );

    expect(written.map(shape)).toEqual([
      'Middle.step - call Inner.fail | FAILED Nms Error: x',
    ]);
  });
});

describe('readable stack', () => {
  // OWN_LOCATION matches a frame located in the tracer or context module,
  // whatever form the runner prints the path in.
  const OWN_LOCATION = /[\\/]src[\\/](spanlog|context)\.ts:\d+/;

  function firstReport(written: Rendered[]): Rendered {
    const rendered = written.find((l) => l.level === 'error');
    if (rendered === undefined) {
      throw new Error('no error line written');
    }
    return rendered;
  }

  function expectOwnFramesHidden(rendered: Rendered): void {
    const readable = rendered.lines.slice(1);
    expect(readable.filter((l) => OWN_LOCATION.test(l))).toEqual([]);
    expect(readable.some((l) => l.includes('AsyncLocalStorage.run'))).toBe(
      false,
    );
    expect(readable.some((l) => l.includes('errors.spec.ts'))).toBe(true);
    expect((rendered.fields.err as ErrorInfo).stack).toMatch(OWN_LOCATION);
  }

  it('hides spanlog frames of an error thrown after an await', async () => {
    class Inner {
      async fail(): Promise<void> {
        await delay(1);
        throw new Error('after await');
      }
    }
    const { spanlog, written } = setup();
    const inner = spanlog.traced(new Inner(), 'Inner');

    await expect(
      spanlog.withRequest('r-10', () => inner.fail()),
    ).rejects.toThrow('after await');

    expectOwnFramesHidden(firstReport(written));
  });

  it('hides spanlog frames of an error thrown before the first await', async () => {
    class Inner {
      async fail(): Promise<void> {
        throw new Error('before await');
      }
    }
    const { spanlog, written } = setup();
    const inner = spanlog.traced(new Inner(), 'Inner');

    await expect(
      spanlog.withRequest('r-11', () => inner.fail()),
    ).rejects.toThrow('before await');

    expectOwnFramesHidden(firstReport(written));
  });

  it('hides spanlog frames of an error thrown inside an explicit call', async () => {
    const { spanlog, written } = setup();

    await expect(
      spanlog.withRequest('r-12', () =>
        spanlog.call('x.y', async () => {
          throw new Error('in call');
        }),
      ),
    ).rejects.toThrow('in call');

    expectOwnFramesHidden(firstReport(written));
  });

  it('keeps the frame of an application method named with the spanlog prefix', async () => {
    class Parser {
      async spanlogParse(): Promise<void> {
        throw new Error('bad input');
      }
    }
    const { spanlog, written } = setup();
    const parser = spanlog.traced(new Parser(), 'Parser');

    await expect(parser.spanlogParse()).rejects.toThrow('bad input');

    const readable = firstReport(written).lines.slice(1);
    expect(readable.some((l) => l.includes('Parser.spanlogParse'))).toBe(true);
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
    const { spanlog, written } = setupAtDebug();
    const repository = spanlog.traced(
      new Repository(new Error(`bad value ${SECRET}`)),
      'Repository',
    );

    await expect(repository.load()).rejects.toThrow(SECRET);

    expect(written.at(-1)?.msg).toContain(SECRET);
  });

  it('replaces the message on every line and in the stack', async () => {
    const { spanlog, written } = setupAtDebug({
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
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
      const { spanlog, written } = setupAtDebug({
        errorMessage: () => 'hidden',
      });
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
      const { spanlog, written } = setupAtDebug({
        errorMessage: () => 'hidden',
      });
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
    const failure = new Error(`bad value\ndetail ${SECRET}`);
    expect(failure.stack).toContain(SECRET);
    failure.message = 'bad value';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.lines.length).toBeGreaterThan(1);
  });

  it('keeps the frames of an Error without a message', async () => {
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
    const repository = spanlog.traced(
      new Repository(new Error('')),
      'Repository',
    );

    await repository.load().catch(() => undefined);

    expect(written.at(-1)?.lines.length).toBeGreaterThan(1);
  });

  it('reads a stack whose first line is the name alone', async () => {
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
    const failure = new Error('');
    failure.stack = 'Error\n    at a (a.js:1:1)';
    const repository = spanlog.traced(new Repository(failure), 'Repository');

    await repository.load().catch(() => undefined);

    const err = written.at(-1)?.fields.err as { stack: string };
    expect(err.stack).toBe('Error: hidden\n    at a (a.js:1:1)');
  });

  it('keeps a frame that holds the text of the message', async () => {
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
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
      const { spanlog, written } = setupAtDebug({
        errorMessage: () => 'hidden',
      });
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
    const repository = spanlog.traced(new Repository(SECRET), 'Repository');

    await expect(repository.load()).rejects.toThrow(SECRET);

    expect(JSON.stringify(written)).not.toContain(SECRET);
  });

  it('hides the message when errorMessage throws', async () => {
    const { spanlog, written } = setupAtDebug({
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
    const { spanlog, written } = setupAtDebug({ errorMessage: () => 'hidden' });
    const repository = spanlog.traced(
      new Repository({ code: 'E1', password: SECRET }),
      'Repository',
    );

    await repository.load().catch(() => undefined);

    expect(JSON.stringify(written)).not.toContain(SECRET);
    expect(written.at(-1)?.msg).toContain('E1');
  });
});
