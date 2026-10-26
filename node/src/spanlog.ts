import {
  currentContext,
  newRequestScope,
  spanlogRunInContext,
  type Frame,
  type RequestScope,
  type SpanContext,
} from './context.js';
import {
  describeThrown,
  errorInfo,
  markReported,
  toFailure,
} from './failure.js';
import { withoutOwnFrames } from './frames.js';
import { redact } from './redact.js';
import { levelFor, render } from './render.js';
import { prettySink, type SpanSink } from './sinks.js';
import type { Action, Level, SpanEvent } from './types.js';

export interface SpanlogOptions {
  /** Service name written on every line. */
  service: string;
  /** Destination of the rendered lines. Defaults to prettySink on stdout. */
  sink?: SpanSink;
  /** Lowest level written. Defaults to info. */
  level?: Level;
  /** Calls slower than this many milliseconds end with SLOW. Defaults to 500. */
  slowMs?: number;
  /**
   * UTC offset of the readable line in minutes. Defaults to the offset of the
   * host time zone at the moment each line is written.
   */
  utcOffsetMinutes?: number;
  /** Keys masked in params and summary on top of DEFAULT_REDACT_KEYS. */
  redactKeys?: string[];
}

export interface CallOptions<T> {
  /** Written, redacted, on the START line. */
  params?: Record<string, unknown>;
  /**
   * Builds the summary written, redacted, on the DONE or SLOW line. Without
   * it an array result is summarised as its length.
   */
  summarize?: (result: T) => Record<string, unknown>;
}

export interface Spanlog {
  /**
   * traced wraps every async method of target, including those inherited
   * from its classes. Each call of a wrapped method writes a START line and an
   * end line. Synchronous methods, getters and setters are left untouched. target is
   * changed in place and returned. Tracing the same object again does
   * nothing.
   */
  traced<T extends object>(target: T, scope: string): T;
  /** call runs fn as an explicit call named target and writes its lines. */
  call<T>(
    target: string,
    fn: () => T | Promise<T>,
    options?: CallOptions<T>,
  ): Promise<T>;
  /**
   * withRequest runs fn as one request. Every line written inside it carries
   * reqId, and each error is reported with its stack once.
   */
  withRequest<T>(reqId: string | undefined, fn: () => T): T;
}

export const DEFAULT_SLOW_MS = 500;

const LEVEL_RANK: Record<Level, number> = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
};

type AsyncMethod = (...args: unknown[]) => Promise<unknown>;

/**
 * Owner is the part of a line that names who writes it and what it is about.
 * scope and method belong to the frame that owns the line. For a wrapped
 * method called from another wrapped method that frame is the caller, and
 * target names the callee.
 */
interface Owner {
  scope?: string;
  method?: string;
  action: Action;
  target?: string;
  sameScope?: boolean;
}

// UnstampedEvent is a SpanEvent before emit adds the time and the service.
type UnstampedEvent = Omit<SpanEvent, 'time' | 'service'>;

// tracedTargets is shared by every Spanlog instance. An object traced by two
// instances would otherwise write every line twice.
const tracedTargets = new WeakSet<object>();

/** getRequestId returns the reqId of the request running now, if any. */
export function getRequestId(): string | undefined {
  return currentContext()?.request.reqId;
}

export function createSpanlog(options: SpanlogOptions): Spanlog {
  // Each option is read once here. A getter on options never runs while a
  // call is being logged.
  const service = options.service;
  const sink = options.sink ?? prettySink();
  const minRank = LEVEL_RANK[options.level ?? 'info'];
  const slowMs = options.slowMs ?? DEFAULT_SLOW_MS;
  const fixedOffsetMinutes = options.utcOffsetMinutes;
  const redactKeys = options.redactKeys ?? [];

  function emit(unstamped: UnstampedEvent): void {
    const event: SpanEvent = { ...unstamped, time: new Date(), service };
    if (LEVEL_RANK[levelFor(event)] < minRank) {
      return;
    }
    const utcOffsetMinutes =
      fixedOffsetMinutes ?? -event.time.getTimezoneOffset();
    try {
      sink.write(
        render(event, { utcOffsetMinutes, stackFilter: withoutOwnFrames }),
      );
    } catch (failure) {
      // A broken sink must never fail the call it describes.
      process.emitWarning(`spanlog sink failed: ${describeThrown(failure)}`, {
        code: 'SPANLOG_SINK',
      });
    }
  }

  function redactRecord(
    value: Record<string, unknown> | undefined,
  ): Record<string, unknown> | undefined {
    if (value === undefined) {
      return undefined;
    }
    return redact(value, { keys: redactKeys }) as Record<string, unknown>;
  }

  async function spanlogTrack<T>(
    owner: Owner,
    request: RequestScope,
    fn: () => T | Promise<T>,
    callOptions?: CallOptions<T>,
  ): Promise<T> {
    const reqId = request.reqId;
    emit({
      ...owner,
      reqId,
      phase: 'START',
      params: paramsForEvent(owner, callOptions),
    });
    const startedAt = performance.now();
    let result: T;
    try {
      result = await fn();
    } catch (thrown) {
      const failure = toFailure(thrown);
      emit({
        ...owner,
        reqId,
        phase: 'FAILED',
        durationMs: performance.now() - startedAt,
        err: errorInfo(failure, redactKeys),
        firstReport: markReported(request, failure),
      });
      throw failure;
    }
    const durationMs = performance.now() - startedAt;
    emit({
      ...owner,
      reqId,
      phase: durationMs > slowMs ? 'SLOW' : 'DONE',
      durationMs,
      summary: summaryForEvent(owner, result, callOptions),
    });
    return result;
  }

  function paramsForEvent<T>(
    owner: Owner,
    callOptions: CallOptions<T> | undefined,
  ): Record<string, unknown> | undefined {
    try {
      return redactRecord(callOptions?.params);
    } catch (failure) {
      // Reading params runs their getters. A getter that throws must not stop
      // the call from running.
      process.emitWarning(
        `spanlog params failed for ${owner.target ?? owner.method}: ${describeThrown(failure)}`,
        { code: 'SPANLOG_PARAMS' },
      );
      return undefined;
    }
  }

  function summaryForEvent<T>(
    owner: Owner,
    result: T,
    callOptions: CallOptions<T> | undefined,
  ): Record<string, unknown> | undefined {
    if (callOptions === undefined) {
      return undefined;
    }
    try {
      return redactRecord(summaryOf(result, callOptions));
    } catch (failure) {
      // A summarize function that throws must not fail a call that succeeded.
      process.emitWarning(
        `spanlog summarize failed for ${owner.target ?? owner.method}: ${describeThrown(failure)}`,
        { code: 'SPANLOG_SUMMARIZE' },
      );
      return undefined;
    }
  }

  // Functions that sit on the stack while application code runs carry the
  // names listed in frames.ts. withoutOwnFrames leaves those frames out of the
  // readable stack. Each arrow function takes its name from the constant it
  // is assigned to.
  function wrapMethod(
    scope: string,
    method: string,
    original: AsyncMethod,
  ): AsyncMethod {
    return function spanlogWrapped(
      this: unknown,
      ...args: unknown[]
    ): Promise<unknown> {
      const parent = currentContext();
      const request = parent?.request ?? newRequestScope(undefined);
      const callee: Frame = { scope, method };
      const context: SpanContext = { request, frame: callee };
      const owner = ownerOf(parent, callee);
      const spanlogInvoke = () => original.apply(this, args);
      const spanlogEnter = () => spanlogTrack(owner, request, spanlogInvoke);
      return spanlogRunInContext(context, spanlogEnter);
    };
  }

  function traced<T extends object>(target: T, scope: string): T {
    if (tracedTargets.has(target)) {
      return target;
    }
    const methods = target as Record<string, unknown>;
    for (const name of asyncMethodNames(target)) {
      const original = methods[name] as AsyncMethod;
      Object.defineProperty(target, name, {
        value: wrapMethod(scope, name, original),
        writable: true,
        configurable: true,
        enumerable: false,
      });
    }
    tracedTargets.add(target);
    return target;
  }

  function spanlogCall<T>(
    target: string,
    fn: () => T | Promise<T>,
    callOptions: CallOptions<T> = {},
  ): Promise<T> {
    const parent = currentContext();
    const request = parent?.request ?? newRequestScope(undefined);
    const frame = parent?.frame;
    const owner: Owner = {
      scope: frame?.scope,
      method: frame?.method,
      action: 'call',
      target,
      sameScope: false,
    };
    // fn runs inside the request. Wrapped methods called from fn share the
    // record of reported errors with this call.
    const spanlogEnter = () => spanlogTrack(owner, request, fn, callOptions);
    return spanlogRunInContext({ request, frame }, spanlogEnter);
  }

  function spanlogWithRequest<T>(reqId: string | undefined, fn: () => T): T {
    return spanlogRunInContext(
      { request: newRequestScope(reqId), frame: undefined },
      fn,
    );
  }

  return { traced, call: spanlogCall, withRequest: spanlogWithRequest };
}

// ownerOf decides whose line a wrapped call writes. Inside another wrapped
// method the line belongs to the caller and names the callee as target.
// Otherwise the callee writes a run line in its own scope.
function ownerOf(parent: SpanContext | undefined, callee: Frame): Owner {
  const caller = parent?.frame;
  if (caller === undefined) {
    return { scope: callee.scope, method: callee.method, action: 'run' };
  }
  return {
    scope: caller.scope,
    method: caller.method,
    action: 'call',
    target: `${callee.scope}.${callee.method}`,
    sameScope: caller.scope === callee.scope,
  };
}

function summaryOf<T>(
  result: T,
  callOptions: CallOptions<T>,
): Record<string, unknown> | undefined {
  if (callOptions.summarize !== undefined) {
    return callOptions.summarize(result);
  }
  return Array.isArray(result) ? { count: result.length } : undefined;
}

// asyncMethodNames walks target and its prototypes up to Object.prototype.
// The nearest definition of a name wins. A synchronous override hides an
// async method of the same name further up. Getters and setters are skipped
// without being called.
function asyncMethodNames(target: object): string[] {
  const seen = new Set<string>();
  const names: string[] = [];
  for (
    let holder: object | null = target;
    holder !== null && holder !== Object.prototype;
    holder = Object.getPrototypeOf(holder) as object | null
  ) {
    for (const name of Object.getOwnPropertyNames(holder)) {
      if (name === 'constructor' || seen.has(name)) {
        continue;
      }
      seen.add(name);
      const descriptor = Object.getOwnPropertyDescriptor(holder, name);
      if (descriptor !== undefined && isAsyncFunction(descriptor.value)) {
        names.push(name);
      }
    }
  }
  return names;
}

// A method that returns a Promise without the async keyword cannot be told
// apart before it runs. Wrapping a synchronous method would turn its result
// into a Promise. Only async functions are wrapped. The tag check also
// works for functions from another realm.
function isAsyncFunction(value: unknown): value is AsyncMethod {
  return (
    typeof value === 'function' &&
    Object.prototype.toString.call(value) === '[object AsyncFunction]'
  );
}
