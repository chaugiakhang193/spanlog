# Examples

Both examples serve `POST /orders` and hold one book in stock. The first order succeeds with a slow payment. The second
fails because the stock is gone. Both write at debug level, so calls within one class are visible.

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

## go-http

Needs Go 1.26. It keeps the stock in memory and has no query lines, because the Go module has none.

```bash
cd examples/go-http
go run .
```

In a second terminal, send the same order twice:

```bash
curl -X POST localhost:3021/orders -d '{"sku":"book","quantity":1}'
```
