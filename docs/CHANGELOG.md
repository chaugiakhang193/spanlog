# Changelog

spanlog follows [Semantic Versioning](https://semver.org). Before 1.0, a minor version can change fields, options and
functions. The npm package, the Go module and the PyPI package share one version number and the log format of
[log-format.md](log-format.md).

## Unreleased

### Docs

- The README of each package shows how to keep pino, slog or `logging` as the logger, and that its level filters the
  lines too.

### Tests

- `pinoSink` is tested against pino 10.4.0.

## 0.1.0 - 2026-11-14

First release.

### Node.js

- `spanlog` on npm. `createSpanlog` with `traced` for services, `call` for single calls and `withRequest`.
- `spanlog/express`: `logRequests` writes one pair of lines per request, with a request id.
- `spanlog/typeorm`: `logQueries` writes query lines with the operation and the table names.
- Sinks for text, JSON and pino.

### Go

- `github.com/chaugiakhang193/spanlog/go`. `New` with `Start` and `Call`, and `Middleware` for `net/http`.
- `Connector` wraps a `database/sql` driver connector and writes query lines.
- Sinks for text, JSON and `log/slog`.

### Python

- `spanlog` on PyPI. `Spanlog` with `traced`, `call` and `acall`.
- `spanlog.asgi` and `spanlog.wsgi`: `LogRequests` middleware for FastAPI, Starlette and Flask.
- `spanlog.sqlalchemy`: `log_queries` for SQLAlchemy 2, with the `sqlalchemy` extra.
- Sinks for text, JSON and `logging`.
