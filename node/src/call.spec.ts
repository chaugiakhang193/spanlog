import { afterEach, describe, expect, it, vi } from 'vitest';
import { createSpanlog } from './spanlog.js';
import { setup, shape } from './testing/helpers.js';
import type { Rendered } from './types.js';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('call', () => {
  it('writes redacted params and counts an array result', async () => {
    const { spanlog, written } = setup();

    const rows = await spanlog.withRequest('r-8', () =>
      spanlog.call('repo.findOrders', async () => [1, 2, 3], {
        params: { customerId: 'c-1', password: 'p' },
      }),
    );

    expect(rows).toEqual([1, 2, 3]);
    expect(written.map(shape)).toEqual([
      'call repo.findOrders | START {"customerId":"c-1","password":"[REDACTED]"}',
      'call repo.findOrders | DONE Nms {"count":3}',
    ]);
  });

  it('writes the call in the frame of the wrapped method around it', async () => {
    const { spanlog, written } = setup();
    class Repo {
      async find(): Promise<string> {
        return spanlog.call('db.find', async () => 'row');
      }
    }
    const repo = spanlog.traced(new Repo(), 'Repo');

    await repo.find();

    expect(written.map(shape)).toEqual([
      'Repo.find - run | START',
      'Repo.find - call db.find | START',
      'Repo.find - call db.find | DONE Nms',
      'Repo.find - run | DONE Nms',
    ]);
  });

  it('shares reported errors with wrapped methods outside a request', async () => {
    const boom = new Error('x');
    const { spanlog, written } = setup();
    class Inner {
      async fail(): Promise<void> {
        throw boom;
      }
    }
    const inner = spanlog.traced(new Inner(), 'Inner');

    await expect(spanlog.call('job.run', () => inner.fail())).rejects.toBe(
      boom,
    );

    expect(written.filter((l) => l.level === 'error').map(shape)).toEqual([
      'Inner.fail - run | FAILED Nms Error: x',
    ]);
  });

  it('keeps the result when summarize throws', async () => {
    const warn = vi.spyOn(process, 'emitWarning').mockImplementation(() => {});
    const { spanlog, written } = setup();

    const value = await spanlog.call('x.y', async () => 1, {
      summarize: () => {
        throw new Error('bad summary');
      },
    });

    expect(value).toBe(1);
    expect(written.map(shape)).toEqual([
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
    const { spanlog, written } = setup();
    const fn = vi.fn(async () => 1);

    await expect(spanlog.call('x.y', fn, { params })).resolves.toBe(1);

    expect(fn).toHaveBeenCalledOnce();
    expect(written.map(shape)).toEqual([
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
    const written: Rendered[] = [];
    const spanlog = createSpanlog({
      service: 'shop',
      sink: { write: (rendered) => written.push(rendered) },
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
    expect(written.map(shape)).toEqual([
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
        write(rendered) {
          if (rendered.fields.phase === 'FAILED') {
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
