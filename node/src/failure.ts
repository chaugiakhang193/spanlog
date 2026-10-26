import { types } from 'node:util';
import type { RequestScope } from './context.js';
import { redact } from './redact.js';
import { stableJson } from './render.js';
import type { ErrorInfo } from './types.js';

const UNREADABLE: ErrorInfo = {
  name: 'NonError',
  message: '[unreadable thrown value]',
};

// toFailure returns a thrown object or function unchanged. Callers catch
// exactly what was thrown. A WeakSet only holds objects. A thrown primitive
// such as a string is therefore wrapped in an Error, and callers further up
// receive the wrapper. String() of a primitive never throws.
export function toFailure(thrown: unknown): object {
  if (
    (typeof thrown === 'object' && thrown !== null) ||
    typeof thrown === 'function'
  ) {
    return thrown;
  }
  return new Error(String(thrown), { cause: thrown });
}

/**
 * describeThrown returns String(value), or a fixed text when String() throws,
 * as it does for an object without a prototype.
 */
export function describeThrown(value: unknown): string {
  try {
    return String(value);
  } catch {
    return '[unprintable value]';
  }
}

// errorInfo copies name, message and stack of an Error one by one. Error
// keeps them as non-enumerable properties, and JSON.stringify(err) prints {}.
// A thrown object that is not an Error becomes a redacted JSON message. A
// thrown function is described by its name only. Reading the thrown value
// runs its getters. A getter that throws leaves UNREADABLE as the description.
export function errorInfo(failure: object, redactKeys: string[]): ErrorInfo {
  try {
    return describe(failure, redactKeys);
  } catch {
    return { ...UNREADABLE };
  }
}

function describe(failure: object, redactKeys: string[]): ErrorInfo {
  if (isError(failure)) {
    const info: ErrorInfo = { name: failure.name, message: failure.message };
    if (failure.stack !== undefined) {
      info.stack = failure.stack;
    }
    return info;
  }
  if (typeof failure === 'function') {
    return {
      name: 'NonError',
      message: `[Function ${failure.name || 'anonymous'}]`,
    };
  }
  return {
    name: 'NonError',
    message: stableJson(redact(failure, { keys: redactKeys })),
  };
}

// An Error created in another realm, such as a vm context, fails instanceof
// Error. Error.isError and util.types.isNativeError both recognise it. Node docs deprecate util.types.isNativeError since 24.2.0. It serves
// runtimes that lack Error.isError.
const isNativeError: (value: unknown) => boolean =
  (Error as { isError?: (value: unknown) => boolean }).isError ??
  types.isNativeError;

function isError(value: object): value is Error {
  return value instanceof Error || isNativeError(value);
}

/** firstReportIn records failure in request and tells whether it is new there. */
export function firstReportIn(request: RequestScope, failure: object): boolean {
  if (request.reported.has(failure)) {
    return false;
  }
  request.reported.add(failure);
  return true;
}
