import { randomBytes } from 'node:crypto';
import type { RequestHandler } from 'express';
import type { Spanlog } from './spanlog.js';

export interface LogRequestsOptions {
  /**
   * Header that carries the request id. An incoming id made of letters,
   * digits, dot, underscore and hyphen, at most 64 characters, is kept.
   * Otherwise a new id is generated. The id is set on the response under the
   * same header. Defaults to x-request-id.
   */
  header?: string;
}

const DEFAULT_REQUEST_ID_HEADER = 'x-request-id';

// The request id lands on every line of the request. An id from a client
// is kept only when it cannot break a line or flood it.
const SAFE_REQUEST_ID = /^[A-Za-z0-9._-]{1,64}$/;

/**
 * logRequests returns an Express middleware that runs each request inside
 * log.withRequest and writes its START and end lines. Register it before the
 * routes. Every line written while they run then carries the reqId.
 */
export function logRequests(
  log: Spanlog,
  options: LogRequestsOptions = {},
): RequestHandler {
  const header = (options.header ?? DEFAULT_REQUEST_ID_HEADER).toLowerCase();
  return function spanlogMiddleware(req, res, next) {
    const reqId = incomingRequestId(req.headers[header]) ?? newRequestId();
    res.setHeader(header, reqId);
    const spanlogEnterRequest = () => {
      const span = log.startRequest(`${req.method} ${pathOf(req.originalUrl)}`);
      // close fires after a complete response and after a dropped
      // connection. writableFinished tells the two apart.
      const writeEnd = () => {
        if (res.writableFinished) {
          span.end(res.statusCode);
        } else {
          span.abort();
        }
      };
      // close does not fire again for a client that left while an earlier
      // async middleware was running.
      if (res.closed) {
        writeEnd();
      } else {
        res.once('close', writeEnd);
      }
      next();
    };
    log.withRequest(reqId, spanlogEnterRequest);
  };
}

function incomingRequestId(
  value: string | string[] | undefined,
): string | undefined {
  return typeof value === 'string' && SAFE_REQUEST_ID.test(value)
    ? value
    : undefined;
}

function newRequestId(): string {
  return randomBytes(4).toString('hex');
}

// The query string often holds search terms or tokens. The line keeps the
// path only.
function pathOf(originalUrl: string): string {
  const queryStart = originalUrl.indexOf('?');
  return queryStart === -1 ? originalUrl : originalUrl.slice(0, queryStart);
}
