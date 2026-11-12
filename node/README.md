# spanlog for Node.js

spanlog writes one line when a call starts and one line when it ends, with its duration, into the logger the application
already uses.

```text
2026-11-12 21:04:32.897 +07:00 INFO  [shop] req=4abd9c49 POST /orders | START
2026-11-12 21:04:32.898 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 run | START
2026-11-12 21:04:32.898 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 call InventoryService.reserve | START
2026-11-12 21:04:32.899 +07:00 DEBUG [shop] [InventoryService.reserve] req=4abd9c49 query UPDATE inventory | START
2026-11-12 21:04:32.902 +07:00 DEBUG [shop] [InventoryService.reserve] req=4abd9c49 query UPDATE inventory | DONE 2.7ms
2026-11-12 21:04:32.904 +07:00 ERROR [shop] [OrderService.handleOrder] req=4abd9c49 call InventoryService.reserve | FAILED 3.9ms Error: stock not available
    at InventoryService.reserve (file:///app/server.js:43:13)
    at process.processTicksAndRejections (node:internal/process/task_queues:104:5)
2026-11-12 21:04:32.905 +07:00 INFO  [shop] [OrderService.handleOrder] req=4abd9c49 run | FAILED 6.6ms Error: stock not available
2026-11-12 21:04:32.906 +07:00 WARN  [shop] req=4abd9c49 POST /orders | FAILED 500 8.6ms
```

This is one failed request from
[`examples/express-orders`](https://github.com/chaugiakhang193/spanlog/blob/main/examples/express-orders/server.js),
without the lines of one internal call. The stack is cut to its first two frames and its paths are shortened.

## Install

The package has no release. Build it from a clone:

```bash
git clone https://github.com/chaugiakhang193/spanlog.git
npm --prefix spanlog/node ci
npm --prefix spanlog/node run build
npm install ./spanlog/node
```

## Quick start

```js
import express from 'express';
import { createSpanlog } from 'spanlog';
import { logRequests } from 'spanlog/express';

class OrderService {
  async handleOrder() {
    return { id: 1 };
  }
}

const log = createSpanlog({ service: 'shop' });
const orders = log.traced(new OrderService(), 'OrderService');

const app = express();
app.use(logRequests(log));
app.post('/orders', async (_req, res) => {
  res.status(201).json(await orders.handleOrder());
});
app.listen(3020, '127.0.0.1');
```

## Documentation

- [Node.js guide](https://github.com/chaugiakhang193/spanlog/blob/main/docs/node.md)
- [Log format](https://github.com/chaugiakhang193/spanlog/blob/main/docs/log-format.md)
- [Examples](https://github.com/chaugiakhang193/spanlog/blob/main/examples/README.md)

## License

[MIT](LICENSE)
