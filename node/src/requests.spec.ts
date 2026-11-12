import { afterEach, describe, expect, it, vi } from 'vitest';
import { getRequestId } from './spanlog.js';
import {
  delay,
  setup,
  setupAtDebug,
  shapeWithLevel,
} from './testing/helpers.js';

describe('startRequest', () => {
  it('writes START and DONE with the status and the reqId', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.withRequest('7f3a9c', () => {
      spanlog.startRequest('POST /orders').end(201);
    });

    expect(written.map(shapeWithLevel)).toEqual([
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
    const { spanlog, written } = setupAtDebug();
    spanlog.withRequest('b81e44', () => {
      spanlog.startRequest('POST /orders').end(500);
    });

    expect(written.map(shapeWithLevel)).toEqual([
      'info POST /orders | START',
      'warn POST /orders | FAILED 500 Nms',
    ]);
  });

  it('keeps a 4xx status as DONE', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.startRequest('GET /orders/9').end(404);

    expect(shapeWithLevel(written[1]!)).toBe(
      'info GET /orders/9 | DONE 404 Nms',
    );
  });

  it('ends with SLOW after slowMs', async () => {
    const { spanlog, written } = setupAtDebug({ slowMs: 5 });
    const span = spanlog.startRequest('GET /report');
    await delay(15);
    span.end(200);

    expect(shapeWithLevel(written[1]!)).toBe('warn GET /report | SLOW 200 Nms');
  });

  it('writes ABORTED without a status', () => {
    const { spanlog, written } = setupAtDebug();
    spanlog.startRequest('GET /stream').abort();

    expect(shapeWithLevel(written[1]!)).toBe('warn GET /stream | ABORTED Nms');
    expect(written[1]?.fields).not.toHaveProperty('status');
  });

  it('writes only the first end line', () => {
    const { spanlog, written } = setupAtDebug();
    const span = spanlog.startRequest('GET /orders');
    span.end(200);
    span.end(500);
    span.abort();

    expect(written.map(shapeWithLevel)).toEqual([
      'info GET /orders | START',
      'info GET /orders | DONE 200 Nms',
    ]);
  });

  it('keeps the reqId of its request when it ends outside the request', () => {
    const { spanlog, written } = setupAtDebug();
    const span = spanlog.withRequest('7f3a9c', () =>
      spanlog.startRequest('GET /orders'),
    );
    span.end(200);

    expect(written[1]?.fields.reqId).toBe('7f3a9c');
  });
});

afterEach(() => {
  vi.restoreAllMocks();
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
