import express from 'express';
import { DataSource, EntitySchema } from 'typeorm';
import { createSpanlog, getRequestId } from 'spanlog';
import { logRequests } from 'spanlog/express';
import { logQueries } from 'spanlog/typeorm';

const HOST = '127.0.0.1';
const PORT = 3020;
const PAYMENT_DELAY_MS = 600;

const Inventory = new EntitySchema({
  name: 'Inventory',
  tableName: 'inventory',
  columns: {
    sku: { type: 'text', primary: true },
    stock: { type: 'int' },
  },
});

const Order = new EntitySchema({
  name: 'Order',
  tableName: 'orders',
  columns: {
    id: { type: 'int', primary: true, generated: true },
    sku: { type: 'text' },
    quantity: { type: 'int' },
  },
});

class InventoryService {
  constructor(dataSource) {
    this.inventory = dataSource.getRepository(Inventory);
  }

  async reserve(sku, quantity) {
    const result = await this.inventory
      .createQueryBuilder()
      .update()
      .set({ stock: () => 'stock - :quantity' })
      .where('sku = :sku AND stock >= :quantity', { sku, quantity })
      .execute();
    if (result.affected === 0) {
      throw new Error('stock not available');
    }
  }
}

class PaymentService {
  async charge() {
    await new Promise((resolve) => setTimeout(resolve, PAYMENT_DELAY_MS));
  }
}

class OrderService {
  constructor(dataSource, inventory, payment) {
    this.orders = dataSource.getRepository(Order);
    this.inventory = inventory;
    this.payment = payment;
  }

  async handleOrder(sku, quantity) {
    await this.validate(sku, quantity);
    await this.inventory.reserve(sku, quantity);
    await this.payment.charge();
    return this.orders.save({ sku, quantity });
  }

  async validate(sku, quantity) {
    if (
      typeof sku !== 'string' ||
      !Number.isInteger(quantity) ||
      quantity <= 0
    ) {
      throw new TypeError(
        'sku must be a string and quantity a positive integer',
      );
    }
  }
}

const log = createSpanlog({ service: 'shop', level: 'debug' });

// dropSchema empties the database on every start. The URL is fixed to the local
// database of compose.yaml for that reason.
const dataSource = new DataSource({
  type: 'postgres',
  url: 'postgres://spanlog:spanlog@127.0.0.1:5438/spanlog',
  entities: [Inventory, Order],
  synchronize: true,
  dropSchema: true,
});
await dataSource.initialize();
await dataSource.getRepository(Inventory).save({ sku: 'book', stock: 1 });
logQueries(dataSource, log);

const orderService = log.traced(
  new OrderService(
    dataSource,
    log.traced(new InventoryService(dataSource), 'InventoryService'),
    log.traced(new PaymentService(), 'PaymentService'),
  ),
  'OrderService',
);

const app = express();
app.use(logRequests(log));
app.use(express.json());

app.post('/orders', async (req, res) => {
  const order = await orderService.handleOrder(req.body.sku, req.body.quantity);
  res.status(201).json(order);
});

// spanlog has already reported the error. This handler only answers the client.
app.use((_error, _req, res, _next) => {
  res.status(500).json({ error: 'order failed', requestId: getRequestId() });
});

app.listen(PORT, HOST);
