# spanlog

Readable call-boundary logs for Node.js and Go.

spanlog writes one line when a call starts and one line when it ends, with its duration, into the logger the
application already uses. Incoming requests, calls between services and database queries share one line format in
Node.js and in Go. There is no collector, dashboard or account to set up.

## Goals

- **Logs, not traces.** Plain lines in the application's own logger, readable in a terminal and searchable with
  `grep "| FAILED"`.
- **One format for Node.js and Go.** Both implementations follow the same specification and are tested against the
  same sample lines.
- **Install once.** A middleware and one wrapper per service in Express. A `Start` call with `defer` in Go.
- **Safe defaults.** Arguments, results and SQL text stay out of the logs unless enabled. Each error is logged once,
  with its stack.

## Status

Early development. The line format, the Node.js package and the Go module have no release.

## License

[MIT](LICENSE)
