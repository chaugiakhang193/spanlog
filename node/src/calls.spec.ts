import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Spanlog } from './spanlog.js';
import { delay, setup, shape } from './testing/helpers.js';

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
    const { spanlog, written } = setup();
    const inventory = spanlog.traced(
      new InventoryService(),
      'InventoryService',
    );

    await spanlog.withRequest('r-1', () => inventory.reserve('A1'));

    expect(written.map(shape)).toEqual([
      'InventoryService.reserve - run | START',
      'InventoryService.reserve - run | DONE Nms',
    ]);
    expect(written[0]?.fields).toMatchObject({
      reqId: 'r-1',
      scope: 'InventoryService',
      method: 'reserve',
      action: 'run',
    });
    expect(written[0]?.lines[0]).toMatch(
      / INFO {2}\[shop\] \[InventoryService\.reserve\] req=r-1 run \| START$/,
    );
  });

  it('writes nested calls in the frame of the caller', async () => {
    const { spanlog, written } = setup({ level: 'debug' });
    const order = wireOrder(spanlog);

    await spanlog.withRequest('r-2', () => order.handleOrder('A1'));

    expect(written.map((l) => `${l.level} ${shape(l)}`)).toEqual([
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
    const { spanlog, written } = setup();
    const order = wireOrder(spanlog);

    await spanlog.withRequest('r-3', () => order.handleOrder('A1'));

    expect(written.some((rendered) => rendered.msg.includes('validate'))).toBe(
      false,
    );
    expect(written).toHaveLength(6);
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
    const { spanlog, written } = setup();
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

    const auditOwners = written
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
    const { spanlog, written } = setup();
    const order = spanlog.traced(new OrderService(), 'OrderService');
    const mixed = spanlog.traced(new Mixed(), 'Mixed');

    expect(order.total(1, 2)).toBe(3);
    expect(Object.hasOwn(order, 'total')).toBe(false);
    expect(reads).toBe(0);
    await mixed.legacy();
    expect(written).toHaveLength(0);
  });

  it('wraps async arrow functions held as fields', async () => {
    class Loader {
      load = async (): Promise<number> => 1;
    }
    const { spanlog, written } = setup();
    const loader = spanlog.traced(new Loader(), 'Loader');

    await loader.load();

    expect(written.map(shape)).toEqual([
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
    const { spanlog, written } = setup();
    const child = spanlog.traced(new Child(), 'Child');

    await child.go();

    expect(written).toHaveLength(0);
  });

  it('does nothing when the same object is traced twice', async () => {
    const { spanlog, written } = setup();
    const inventory = spanlog.traced(
      new InventoryService(),
      'InventoryService',
    );
    spanlog.traced(inventory, 'InventoryService');

    await inventory.reserve('A1');

    expect(written).toHaveLength(2);
  });

  it('ends a slow call with SLOW at warn', async () => {
    class Slow {
      async wait(): Promise<void> {
        await delay(10);
      }
    }
    const { spanlog, written } = setup({ slowMs: 1 });
    const slow = spanlog.traced(new Slow(), 'Slow');

    await slow.wait();

    expect(written[1]?.fields.phase).toBe('SLOW');
    expect(written[1]?.level).toBe('warn');
  });

  it('writes lines outside a request without reqId', async () => {
    const { spanlog, written } = setup();
    const inventory = spanlog.traced(
      new InventoryService(),
      'InventoryService',
    );

    await inventory.reserve('A1');

    expect(written[0]?.fields.reqId).toBeUndefined();
    expect(written[0]?.lines[0]).toContain(
      ' [shop] [InventoryService.reserve] req=- run | START',
    );
  });
});
