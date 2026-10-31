import { once } from 'node:events';
import { request as httpRequest, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import express, { type ErrorRequestHandler, type Express } from 'express';
import { afterEach, describe, expect, it } from 'vitest';
import { logRequests } from './express.js';
import { createSpanlog, type Spanlog } from './spanlog.js';
import type { Rendered } from './types.js';

const MS = /\d+(\.\d+)?ms/;

function shape(rendered: Rendered): string {
  const reqId = String(rendered.fields.reqId ?? '-');
  return `${reqId} ${rendered.level} ${rendered.msg.replace(MS, 'Nms')}`;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

class OrderService {
  async place(sku: string): Promise<string> {
    await delay(5);
    if (sku === 'none') {
      throw new Error('stock not available');
    }
    return `order for ${sku}`;
  }

  // refuse throws before its first await. The stack still holds every caller.
  async refuse(): Promise<void> {
    throw new Error('order refused');
  }
}

const servers: Server[] = [];

afterEach(async () => {
  for (const server of servers.splice(0)) {
    server.closeAllConnections();
    server.close();
    await once(server, 'close');
  }
});

interface Setup {
  log: Spanlog;
  written: Rendered[];
  baseUrl: string;
  /** Resolves when a line whose msg contains text is written. */
  lineWith: (text: string) => Promise<Rendered>;
}

// setup builds the app the way a user would: the middleware first, a traced
// service, routes, then an error handler.
async function setup(
  routes: (app: Express, orders: OrderService) => void,
): Promise<Setup> {
  const written: Rendered[] = [];
  const waiters: { text: string; resolve: (line: Rendered) => void }[] = [];
  const log = createSpanlog({
    service: 'shop',
    utcOffsetMinutes: 0,
    sink: {
      write: (rendered) => {
        written.push(rendered);
        for (const waiter of waiters) {
          if (rendered.msg.includes(waiter.text)) waiter.resolve(rendered);
        }
      },
    },
  });
  const orders = log.traced(new OrderService(), 'OrderService');
  const app = express();
  app.use(logRequests(log));
  routes(app, orders);
  const sendStatus500: ErrorRequestHandler = (_err, _req, res, _next) => {
    res.status(500).json({ error: 'internal' });
  };
  app.use(sendStatus500);

  const server = app.listen(0, '127.0.0.1');
  servers.push(server);
  await once(server, 'listening');
  const { port } = server.address() as AddressInfo;
  const lineWith = (text: string) => {
    const found = written.find((line) => line.msg.includes(text));
    if (found !== undefined) return Promise.resolve(found);
    return new Promise<Rendered>((resolve) => waiters.push({ text, resolve }));
  };
  return { log, written, baseUrl: `http://127.0.0.1:${port}`, lineWith };
}

function orderRoutes(app: Express, orders: OrderService): void {
  app.post('/orders/:sku', async (req, res) => {
    res.status(201).json({ order: await orders.place(req.params.sku) });
  });
}

describe('logRequests', () => {
  it('writes request lines around the traced calls of the route', async () => {
    const { written, baseUrl, lineWith } = await setup(orderRoutes);

    const response = await fetch(`${baseUrl}/orders/A1?coupon=FREE`, {
      method: 'POST',
    });
    await lineWith('| DONE 201');

    const reqId = response.headers.get('x-request-id');
    expect(reqId).toMatch(/^[0-9a-f]{8}$/);
    expect(written.map(shape)).toEqual([
      `${reqId} info POST /orders/A1 | START`,
      `${reqId} info OrderService.place - run | START`,
      `${reqId} info OrderService.place - run | DONE Nms`,
      `${reqId} info POST /orders/A1 | DONE 201 Nms`,
    ]);
  });

  it('keeps a safe incoming request id', async () => {
    const { written, baseUrl, lineWith } = await setup(orderRoutes);

    const response = await fetch(`${baseUrl}/orders/A1`, {
      method: 'POST',
      headers: { 'x-request-id': 'edge-42.a_b' },
    });
    await lineWith('| DONE 201');

    expect(response.headers.get('x-request-id')).toBe('edge-42.a_b');
    expect(written.every((line) => line.fields.reqId === 'edge-42.a_b')).toBe(
      true,
    );
  });

  it('replaces an incoming id that could break the line', async () => {
    const { baseUrl, lineWith } = await setup(orderRoutes);

    const response = await fetch(`${baseUrl}/orders/A1`, {
      method: 'POST',
      headers: { 'x-request-id': `a b ${'x'.repeat(70)}` },
    });
    const done = await lineWith('| DONE 201');

    expect(response.headers.get('x-request-id')).toMatch(/^[0-9a-f]{8}$/);
    expect(done.fields.reqId).toBe(response.headers.get('x-request-id'));
  });

  it('ends with FAILED 500 and reports the error once on the run line', async () => {
    const { written, baseUrl, lineWith } = await setup(orderRoutes);

    const response = await fetch(`${baseUrl}/orders/none`, { method: 'POST' });
    await lineWith('| FAILED 500');

    expect(response.status).toBe(500);
    const reqId = response.headers.get('x-request-id');
    expect(written.map(shape)).toEqual([
      `${reqId} info POST /orders/none | START`,
      `${reqId} info OrderService.place - run | START`,
      `${reqId} error OrderService.place - run | FAILED Nms Error: stock not available`,
      `${reqId} warn POST /orders/none | FAILED 500 Nms`,
    ]);
    expect(written[2]?.lines.length).toBeGreaterThan(1);
  });

  it('keeps the reqId of each request apart when they overlap', async () => {
    const { written, baseUrl, lineWith } = await setup(orderRoutes);

    const responses = await Promise.all(
      ['A1', 'B2', 'C3'].map((sku) =>
        fetch(`${baseUrl}/orders/${sku}`, { method: 'POST' }),
      ),
    );
    await Promise.all(
      ['A1', 'B2', 'C3'].map((sku) => lineWith(`/orders/${sku} | DONE`)),
    );

    for (const [index, sku] of ['A1', 'B2', 'C3'].entries()) {
      const reqId = responses[index]?.headers.get('x-request-id');
      const own = written.filter((line) => line.fields.reqId === reqId);
      expect(own.map(shape)).toEqual([
        `${reqId} info POST /orders/${sku} | START`,
        `${reqId} info OrderService.place - run | START`,
        `${reqId} info OrderService.place - run | DONE Nms`,
        `${reqId} info POST /orders/${sku} | DONE 201 Nms`,
      ]);
    }
  });

  it('writes ABORTED when the client drops the connection', async () => {
    let arrived!: () => void;
    const requestArrived = new Promise<void>((resolve) => {
      arrived = resolve;
    });
    const { baseUrl, lineWith } = await setup((app) => {
      app.get('/stream', (_req, res) => {
        res.write('first chunk');
        arrived();
      });
    });

    const client = httpRequest(`${baseUrl}/stream`);
    client.on('error', () => {});
    client.end();
    await requestArrived;
    client.destroy();
    const aborted = await lineWith('| ABORTED');

    expect(shape(aborted)).toMatch(
      /^[0-9a-f]{8} warn GET \/stream \| ABORTED Nms$/,
    );
    expect(aborted.fields).not.toHaveProperty('status');
  });

  it('writes ABORTED when the client left before the middleware ran', async () => {
    const written: Rendered[] = [];
    const log = createSpanlog({
      service: 'shop',
      sink: { write: (rendered) => written.push(rendered) },
    });
    let arrived!: () => void;
    const requestArrived = new Promise<void>((resolve) => {
      arrived = resolve;
    });
    let clientLeft!: () => void;
    const clientHasLeft = new Promise<void>((resolve) => {
      clientLeft = resolve;
    });
    let middlewareDone!: () => void;
    const middlewareFinished = new Promise<void>((resolve) => {
      middlewareDone = resolve;
    });
    const app = express();
    app.use(async (_req, res, next) => {
      res.once('close', clientLeft);
      arrived();
      await clientHasLeft;
      next();
    });
    app.use(logRequests(log));
    app.use(() => middlewareDone());
    const server = app.listen(0, '127.0.0.1');
    servers.push(server);
    await once(server, 'listening');
    const { port } = server.address() as AddressInfo;

    const client = httpRequest(`http://127.0.0.1:${port}/late`);
    client.on('error', () => {});
    client.end();
    await requestArrived;
    client.destroy();
    await middlewareFinished;

    expect(
      written.map(shape).map((line) => line.split(' ').slice(1).join(' ')),
    ).toEqual(['info GET /late | START', 'warn GET /late | ABORTED Nms']);
  });

  it('leaves the middleware frames out of a deep readable stack', async () => {
    const defaultLimit = Error.stackTraceLimit;
    Error.stackTraceLimit = 50;
    try {
      const { baseUrl, lineWith } = await setup((app, orders) => {
        app.get('/fail', (_req, res) => {
          orders.refuse().catch(() => res.status(500).end());
        });
      });

      await fetch(`${baseUrl}/fail`);
      const failed = await lineWith('OrderService.refuse - run | FAILED');

      const err = failed.fields.err as { stack: string };
      expect(err.stack).toContain('spanlogMiddleware');
      expect(err.stack).toContain('spanlogEnterRequest');
      expect(failed.lines.join('\n')).not.toMatch(
        /spanlogMiddleware|spanlogEnterRequest/,
      );
    } finally {
      Error.stackTraceLimit = defaultLimit;
    }
  });

  it('uses the header given in the options', async () => {
    const written: Rendered[] = [];
    const log = createSpanlog({
      service: 'shop',
      sink: { write: (rendered) => written.push(rendered) },
    });
    const app = express();
    app.use(logRequests(log, { header: 'X-Trace-Id' }));
    app.get('/ping', (_req, res) => {
      res.send('pong');
    });
    const server = app.listen(0, '127.0.0.1');
    servers.push(server);
    await once(server, 'listening');
    const { port } = server.address() as AddressInfo;

    const response = await fetch(`http://127.0.0.1:${port}/ping`, {
      headers: { 'x-trace-id': 'trace-1' },
    });
    await response.text();

    expect(response.headers.get('x-trace-id')).toBe('trace-1');
    expect(response.headers.has('x-request-id')).toBe(false);
  });
});
