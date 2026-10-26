import { AsyncLocalStorage } from 'node:async_hooks';

/** Frame is the wrapped method that is running. */
export interface Frame {
  scope: string;
  method: string;
}

/**
 * RequestScope is shared by every line written while one request runs.
 * reported holds the errors already marked as reported in the request. A
 * failure is marked before its line reaches the level filter and the sink.
 */
export interface RequestScope {
  reqId: string | undefined;
  reported: WeakSet<object>;
}

export interface SpanContext {
  request: RequestScope;
  frame: Frame | undefined;
}

// Each wrapped call runs in a new SpanContext object. Two calls running side
// by side in one request, such as the two halves of a Promise.all, therefore
// see their own frame and share the same RequestScope.
const storage = new AsyncLocalStorage<SpanContext>();

export function currentContext(): SpanContext | undefined {
  return storage.getStore();
}

/**
 * spanlogRunInContext runs fn with context as the current SpanContext. The
 * spanlog prefix keeps its frame out of the readable stack.
 */
export function spanlogRunInContext<T>(context: SpanContext, fn: () => T): T {
  return storage.run(context, fn);
}

export function newRequestScope(reqId: string | undefined): RequestScope {
  return { reqId, reported: new WeakSet() };
}
