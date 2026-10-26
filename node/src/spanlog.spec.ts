import { runInNewContext } from 'node:vm';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  createSpanlog,
  getRequestId,
  type Spanlog,
  type SpanlogOptions,
} from './spanlog.js';
import type { ErrorInfo, Rendered } from './types.js';

const MS = /\d+(\.\d+)?ms/;

function setup(options: Partial<SpanlogOptions> = {}) {
  const lines: Rendered[] = [];
  const spanlog = createSpanlog({
    service: 'shop',
    sink: { write: (line) => lines.push(line) },
    utcOffsetMinutes: 0,
    ...options,
  });
  return { spanlog, lines };
}

// shape hides the measured duration, which changes from run to run.
function shape(line: Rendered): string {
  return line.msg.replace(MS, 'Nms');
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

class InventoryService {
  async reserve(sku: string): Promise<number> {
    await delay(1);
    return sku.length;
  }
}

class PaymentService {
  async charge(amount: number): Promise<string> {
    await delay(1);
    return `paid ${amount}`;
  }
}

class OrderService {
  inventory!: InventoryService;
  payment!: PaymentService;

  async handleOrder(sku: string): Promise<string> {
    await this.validate(sku);
    await this.inventory.reserve(sku);
    return this.payment.charge(10);
  }

  async validate(sku: string): Promise<void> {
    if (sku === '') {
      throw new Error('empty sku');
    }
  }

  total(a: number, b: number): number {
    return a + b;
  }
}

function wireOrder(spanlog: Spanlog): OrderService {
  const order = spanlog.traced(new OrderService(), 'OrderService');
  order.inventory = spanlog.traced(new InventoryService(), 'InventoryService');
  order.payment = spanlog.traced(new PaymentService(), 'PaymentService');
  return order;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('traced', () => {
  it('writes a run pair carrying the request id', async () => {
    const { spanlog, lines } = setup();
    const inventory = spanlog.traced(
      new InventoryService(),
      'InventoryService',
    );

    await spanlog.withRequest('r-1', () => inventory.reserve('A1'));

    expect(lines.map(shape)).toEqual([
      'InventoryService.reserve - run | START',
      'InventoryService.reserve - run | DONE Nms',
    ]);
    expect(lines[0]?.fields).toMatchObject({
      reqId: 'r-1',
      scope: 'InventoryService',
      method: 'reserve',
      action: 'run',
    });
    expect(lines[0]?.lines[0]).toMatch(
      / INFO {2}\[shop\] \[InventoryService\.reserve\] req=r-1 run \| START$/,
    );
  });

  it('writes nested calls in the frame of the caller', async () => {
    const { spanlog, lines } = setup({ level: 'debug' });
    const order = wireOrder(spanlog);

    await spanlog.withRequest('r-2', () => order.handleOrder('A1'));

    expect(lines.map((l) => `${l.level} ${shape(l)}`)).toEqual([
      'info OrderService.handleOrder - run | START',
      'debug OrderService.handleOrder - call OrderService.validate | START',
      'debug OrderService.handleOrder - call OrderService.validate | DONE Nms',
      'info OrderService.handleOrder - call InventoryService.reserve | START',
      'info OrderService.handleOrder - call InventoryService.reserve | DONE Nms',
      'info OrderService.handleOrder - call PaymentService.charge | START',
      'info OrderService.handleOrder - call PaymentService.charge | DONE Nms',
      'info OrderService.handleOrder - run | DONE Nms',
    ]);
  });

  it('hides calls within one scope at the default level', async () => {
    const { spanlog, lines } = setup();
    const order = wireOrder(spanlog);

    await spanlog.withRequest('r-3', () => order.handleOrder('A1'));

    expect(lines.some((line) => line.msg.includes('validate'))).toBe(false);
    expect(lines).toHaveLength(6);
  });

  it('keeps the frame of each branch of a Promise.all', async () => {
    class AuditLog {
      async write(): Promise<void> {
        await delay(1);
      }
    }
    class SlowInventory {
      audit!: AuditLog;
      async reserve(): Promise<void> {
        await delay(5);
        await this.audit.write();
      }
    }
    class FastPayment {
      audit!: AuditLog;
      async charge(): Promise<void> {
        await delay(1);
        await this.audit.write();
      }
    }
    class Checkout {
      inventory!: SlowInventory;
      payment!: FastPayment;
      async run(): Promise<void> {
        await Promise.all([this.inventory.reserve(), this.payment.charge()]);
      }
    }
    const { spanlog, lines } = setup();
    const audit = spanlog.traced(new AuditLog(), 'AuditLog');
    const checkout = spanlog.traced(new Checkout(), 'Checkout');
    checkout.inventory = spanlog.traced(
      new SlowInventory(),
      'InventoryService',
    );
    checkout.payment = spanlog.traced(new FastPayment(), 'PaymentService');
    checkout.inventory.audit = audit;
    checkout.payment.audit = audit;

    await spanlog.withRequest('r-4', () => checkout.run());

    const auditOwners = lines
      .filter(
        (l) =>
          l.fields.target === 'AuditLog.write' && l.fields.phase === 'START',
      )
      .map((l) => `${l.fields.scope}.${l.fields.method}`);
    expect(auditOwners).toEqual([
      'PaymentService.charge',
      'InventoryService.reserve',
    ]);
  });

  it('leaves synchronous methods, getters and promise-returning plain methods alone', async () => {
    let reads = 0;
    class Mixed {
      get count(): number {
        reads += 1;
        return 1;
      }
      legacy(): Promise<number> {
        return Promise.resolve(1);
      }
    }
    const { spanlog, lines } = setup();
    const order = spanlog.traced(new OrderService(), 'OrderService');
    const mixed = spanlog.traced(new Mixed(), 'Mixed');

    expect(order.total(1, 2)).toBe(3);
    expect(Object.hasOwn(order, 'total')).toBe(false);
    expect(reads).toBe(0);
    await mixed.legacy();
    expect(lines).toHaveLength(0);
  });

  it('wraps async arrow functions held as fields', async () => {
    class Loader {
      load = async (): Promise<number> => 1;
    }
    const { spanlog, lines } = setup();
    const loader = spanlog.traced(new Loader(), 'Loader');

    await loader.load();

    expect(lines.map(shape)).toEqual([
      'Loader.load - run | START',
      'Loader.load - run | DONE Nms',
    ]);
  });

  it('lets a synchronous override hide an async parent method', async () => {
    class Base {
      async go(): Promise<number> {
        return 1;
      }
    }
    class Child extends Base {
      override go(): Promise<number> {
        return Promise.resolve(2);
      }
    }
    const { spanlog, lines } = setup();
    const child = spanlog.traced(new Child(), 'Child');

    await child.go();

    expect(lines).toHaveLength(0);
  });

  it('does nothing when the same object is traced twice', async () => {
    const { spanlog, lines } = setup();
    const inventory = spanlog.traced(
      new InventoryService(),
      'InventoryService',
    );
    spanlog.traced(inventory, 'InventoryService');

    await inventory.reserve('A1');

    expect(lines).toHaveLength(2);
  });

  it('ends a slow call with SLOW at warn', async () => {
    class Slow {
      async wait(): Promise<void> {
        await delay(10);
      }
    }
    const { spanlog, lines } = setup({ slowMs: 1 });
    const slow = spanlog.traced(new Slow(), 'Slow');

    await slow.wait();

    expect(lines[1]?.fields.phase).toBe('SLOW');
    expect(lines[1]?.level).toBe('warn');
  });

  it('writes lines outside a request without reqId', async () => {
    const { spanlog, lines } = setup();
    const inventory = spanlog.traced(
      new InventoryService(),
      'InventoryService',
    );

    await inventory.reserve('A1');

    expect(lines[0]?.fields.reqId).toBeUndefined();
    expect(lines[0]?.lines[0]).toContain(
      ' [shop] [InventoryService.reserve] req=- run | START',
    );
  });
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
    const { spanlog, lines } = setup();
    const outer = wireChain(spanlog, boom);

    await expect(spanlog.withRequest('r-5', () => outer.run())).rejects.toBe(
      boom,
    );

    const failed = lines.filter((l) => l.fields.phase === 'FAILED');
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
    const { spanlog, lines } = setup();
    const outer = wireChain(spanlog, shared);

    await Promise.allSettled([
      spanlog.withRequest('a', () => outer.run()),
      spanlog.withRequest('b', () => outer.run()),
    ]);

    const errors = lines.filter((l) => l.level === 'error');
    expect(errors.map((l) => l.fields.reqId).sort()).toEqual(['a', 'b']);
  });

  it('wraps a thrown string in an Error and rethrows the wrapper', async () => {
    const { spanlog, lines } = setup();
    const outer = wireChain(spanlog, 'plain text');

    const outcome = spanlog.withRequest('r-6', () => outer.run());

    await expect(outcome).rejects.toBeInstanceOf(Error);
    await expect(outcome).rejects.toMatchObject({
      message: 'plain text',
      cause: 'plain text',
    });
    expect(lines.filter((l) => l.level === 'error')).toHaveLength(1);
  });

  it('describes a thrown plain object by its redacted JSON', async () => {
    const { spanlog, lines } = setup();
    const outer = wireChain(spanlog, { code: 'E1', token: 't' });

    await expect(outer.run()).rejects.toEqual({ code: 'E1', token: 't' });

    expect(lines.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'NonError',
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
    const { spanlog, lines } = setup();
    const outer = wireChain(spanlog, thrown);

    await expect(outer.run()).rejects.toBe(thrown);

    expect(lines.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'NonError',
      message: '[unreadable thrown value]',
    });
  });

  it('describes an Error from another realm by its name, message and stack', async () => {
    const foreign = runInNewContext('new RangeError("realm failure")') as {
      stack: string;
    };
    const { spanlog, lines } = setup();
    const outer = wireChain(spanlog, foreign);

    await expect(outer.run()).rejects.toBe(foreign);

    expect(lines.find((l) => l.level === 'error')?.fields.err).toEqual({
      name: 'RangeError',
      message: 'realm failure',
      stack: foreign.stack,
    });
  });

  it('keeps counting reports when the level hides the outer lines', async () => {
    const { spanlog, lines } = setup({ level: 'error' });
    const outer = wireChain(spanlog, new Error('x'));

    await expect(spanlog.withRequest('r-7', () => outer.run())).rejects.toThrow(
      'x',
    );

    expect(lines.map(shape)).toEqual([
      'Middle.step - call Inner.fail | FAILED Nms Error: x',
    ]);
  });
});

describe('readable stack', () => {
  // OWN_LOCATION matches a frame located in the tracer or context module,
  // whatever form the runner prints the path in.
  const OWN_LOCATION = /[\\/]src[\\/](spanlog|context)\.ts:\d+/;

  function firstReport(lines: Rendered[]): Rendered {
    const line = lines.find((l) => l.level === 'error');
    if (line === undefined) {
      throw new Error('no error line written');
    }
    return line;
  }

  function expectOwnFramesHidden(line: Rendered): void {
    const readable = line.lines.slice(1);
    expect(readable.filter((l) => OWN_LOCATION.test(l))).toEqual([]);
    expect(readable.some((l) => l.includes('AsyncLocalStorage.run'))).toBe(
      false,
    );
    expect(readable.some((l) => l.includes('spanlog.spec.ts'))).toBe(true);
    expect((line.fields.err as ErrorInfo).stack).toMatch(OWN_LOCATION);
  }

  it('hides spanlog frames of an error thrown after an await', async () => {
    class Inner {
      async fail(): Promise<void> {
        await delay(1);
        throw new Error('after await');
      }
    }
    const { spanlog, lines } = setup();
    const inner = spanlog.traced(new Inner(), 'Inner');

    await expect(
      spanlog.withRequest('r-10', () => inner.fail()),
    ).rejects.toThrow('after await');

    expectOwnFramesHidden(firstReport(lines));
  });

  it('hides spanlog frames of an error thrown before the first await', async () => {
    class Inner {
      async fail(): Promise<void> {
        throw new Error('before await');
      }
    }
    const { spanlog, lines } = setup();
    const inner = spanlog.traced(new Inner(), 'Inner');

    await expect(
      spanlog.withRequest('r-11', () => inner.fail()),
    ).rejects.toThrow('before await');

    expectOwnFramesHidden(firstReport(lines));
  });

  it('hides spanlog frames of an error thrown inside an explicit call', async () => {
    const { spanlog, lines } = setup();

    await expect(
      spanlog.withRequest('r-12', () =>
        spanlog.call('x.y', async () => {
          throw new Error('in call');
        }),
      ),
    ).rejects.toThrow('in call');

    expectOwnFramesHidden(firstReport(lines));
  });

  it('keeps the frame of an application method named with the spanlog prefix', async () => {
    class Parser {
      async spanlogParse(): Promise<void> {
        throw new Error('bad input');
      }
    }
    const { spanlog, lines } = setup();
    const parser = spanlog.traced(new Parser(), 'Parser');

    await expect(parser.spanlogParse()).rejects.toThrow('bad input');

    const readable = firstReport(lines).lines.slice(1);
    expect(readable.some((l) => l.includes('Parser.spanlogParse'))).toBe(true);
  });
});

describe('call', () => {
  it('writes redacted params and counts an array result', async () => {
    const { spanlog, lines } = setup();

    const rows = await spanlog.withRequest('r-8', () =>
      spanlog.call('repo.findOrders', async () => [1, 2, 3], {
        params: { customerId: 'c-1', password: 'p' },
      }),
    );

    expect(rows).toEqual([1, 2, 3]);
    expect(lines.map(shape)).toEqual([
      'call repo.findOrders | START {"customerId":"c-1","password":"[REDACTED]"}',
      'call repo.findOrders | DONE Nms {"count":3}',
    ]);
  });

  it('writes the call in the frame of the wrapped method around it', async () => {
    const { spanlog, lines } = setup();
    class Repo {
      async find(): Promise<string> {
        return spanlog.call('db.find', async () => 'row');
      }
    }
    const repo = spanlog.traced(new Repo(), 'Repo');

    await repo.find();

    expect(lines.map(shape)).toEqual([
      'Repo.find - run | START',
      'Repo.find - call db.find | START',
      'Repo.find - call db.find | DONE Nms',
      'Repo.find - run | DONE Nms',
    ]);
  });

  it('shares reported errors with wrapped methods outside a request', async () => {
    const boom = new Error('x');
    const { spanlog, lines } = setup();
    class Inner {
      async fail(): Promise<void> {
        throw boom;
      }
    }
    const inner = spanlog.traced(new Inner(), 'Inner');

    await expect(spanlog.call('job.run', () => inner.fail())).rejects.toBe(
      boom,
    );

    expect(lines.filter((l) => l.level === 'error').map(shape)).toEqual([
      'Inner.fail - run | FAILED Nms Error: x',
    ]);
  });

  it('keeps the result when summarize throws', async () => {
    const warn = vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const { spanlog, lines } = setup();

    const value = await spanlog.call('x.y', async () => 1, {
      summarize: () => {
        throw new Error('bad summary');
      },
    });

    expect(value).toBe(1);
    expect(lines.map(shape)).toEqual([
      'call x.y | START',
      'call x.y | DONE Nms',
    ]);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('summarize failed for x.y'),
      { code: 'SPANLOG_SUMMARIZE' },
    );
  });

  it('keeps the result when the sink throws', async () => {
    const warn = vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const spanlog = createSpanlog({
      service: 'shop',
      sink: {
        write() {
          throw new Error('disk full');
        },
      },
    });

    await expect(spanlog.call('x.y', async () => 1)).resolves.toBe(1);

    expect(warn).toHaveBeenCalledWith(expect.stringContaining('disk full'), {
      code: 'SPANLOG_SINK',
    });
  });

  it('runs fn and keeps the result when the sink throws a value String() rejects', async () => {
    const warn = vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const spanlog = createSpanlog({
      service: 'shop',
      sink: {
        write() {
          throw Object.create(null);
        },
      },
    });
    const fn = vi.fn(async () => 1);

    await expect(spanlog.call('x.y', fn)).resolves.toBe(1);

    expect(fn).toHaveBeenCalledOnce();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('spanlog sink failed'),
      { code: 'SPANLOG_SINK' },
    );
  });

  it('keeps the result when summarize throws a value String() rejects', async () => {
    const warn = vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const { spanlog } = setup();

    const value = await spanlog.call('x.y', async () => 1, {
      summarize: () => {
        throw Object.create(null);
      },
    });

    expect(value).toBe(1);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('summarize failed for x.y'),
      { code: 'SPANLOG_SUMMARIZE' },
    );
  });

  it('runs fn and leaves params out when a params getter throws', async () => {
    const warn = vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const params = {};
    Object.defineProperty(params, 'orderId', {
      enumerable: true,
      get() {
        throw new Error('params getter failed');
      },
    });
    const { spanlog, lines } = setup();
    const fn = vi.fn(async () => 1);

    await expect(spanlog.call('x.y', fn, { params })).resolves.toBe(1);

    expect(fn).toHaveBeenCalledOnce();
    expect(lines.map(shape)).toEqual([
      'call x.y | START',
      'call x.y | DONE Nms',
    ]);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining(
        'params failed for x.y: Error: params getter failed',
      ),
      { code: 'SPANLOG_PARAMS' },
    );
  });

  it('reads utcOffsetMinutes once, when the Spanlog is created', async () => {
    let reads = 0;
    const lines: Rendered[] = [];
    const spanlog = createSpanlog({
      service: 'shop',
      sink: { write: (line) => lines.push(line) },
      get utcOffsetMinutes() {
        reads += 1;
        if (reads > 1) {
          throw new Error('offset read again');
        }
        return 0;
      },
    });
    const boom = new Error('payment declined');

    await expect(spanlog.call('x.y', async () => 42)).resolves.toBe(42);
    await expect(
      spanlog.call('x.y', async () => {
        throw boom;
      }),
    ).rejects.toBe(boom);

    expect(reads).toBe(1);
    expect(lines.map(shape)).toEqual([
      'call x.y | START',
      'call x.y | DONE Nms',
      'call x.y | START',
      'call x.y | FAILED Nms Error: payment declined',
    ]);
  });

  it('throws from createSpanlog when the utcOffsetMinutes getter throws', () => {
    expect(() =>
      createSpanlog({
        service: 'shop',
        sink: { write() {} },
        get utcOffsetMinutes(): number {
          throw new Error('offset unavailable');
        },
      }),
    ).toThrow('offset unavailable');
  });

  it('rethrows the error of fn when the sink throws on the FAILED line', async () => {
    vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const boom = new Error('payment declined');
    const spanlog = createSpanlog({
      service: 'shop',
      sink: {
        write(line) {
          if (line.fields.phase === 'FAILED') {
            throw Object.create(null);
          }
        },
      },
    });

    await expect(
      spanlog.call('x.y', async () => {
        throw boom;
      }),
    ).rejects.toBe(boom);
  });
});

describe('getRequestId', () => {
  it('returns the reqId inside withRequest only', async () => {
    const { spanlog } = setup();

    const inside = await spanlog.withRequest('r-9', async () => {
      await delay(1);
      return getRequestId();
    });

    expect(inside).toBe('r-9');
    expect(getRequestId()).toBeUndefined();
  });
});
