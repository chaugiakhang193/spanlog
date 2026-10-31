# Examples

The example serves `POST /orders` and holds one book in stock. The first order succeeds with a slow payment. The
second fails because the stock is gone. It writes at debug level, so calls within one class are visible.

## express-orders

Needs Node.js 22.13 or newer and Docker. The example empties the database of `compose.yaml` every time it starts.

```bash
docker compose up -d --wait
npm --prefix node ci
npm --prefix node run build
npm --prefix examples/express-orders install
npm --prefix examples/express-orders start
```

In a second terminal, send the same order twice:

```bash
curl -X POST localhost:3020/orders -H 'content-type: application/json' -d '{"sku":"book","quantity":1}'
```

`docker compose down` stops the database.
