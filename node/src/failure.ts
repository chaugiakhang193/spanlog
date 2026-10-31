import { types } from 'node:util';
import type { RequestScope } from './context.js';
import { redact } from './redact.js';
import { stableJson } from './render.js';
import type { ErrorInfo } from './types.js';

// THROWN_VALUE is the err.name of a thrown object or function that is not an
// Error. UNREADABLE stands in when reading the thrown value throws, for an Error
// as well. The call ends with FAILED in both cases.
const THROWN_VALUE = 'ThrownValue';
const UNREADABLE: ErrorInfo = {
  name: 'UnreadableError',
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
// runs its getters. A getter that throws leaves UNREADABLE as the description,
// and so does an errorMessage function that throws.
export function errorInfo(
  failure: object,
  redactKeys: string[],
  errorMessage?: (error: Error) => string,
): ErrorInfo {
  try {
    return readErrorInfo(failure, redactKeys, errorMessage);
  } catch {
    return { ...UNREADABLE };
  }
}

function readErrorInfo(
  failure: object,
  redactKeys: string[],
  errorMessage: ((error: Error) => string) | undefined,
): ErrorInfo {
  if (isError(failure)) {
    if (errorMessage !== undefined) {
      return replacedErrorInfo(failure, errorMessage(failure));
    }
    const info: ErrorInfo = { name: failure.name, message: failure.message };
    if (failure.stack !== undefined) {
      info.stack = failure.stack;
    }
    return info;
  }
  if (typeof failure === 'function') {
    return {
      name: THROWN_VALUE,
      message: `[Function ${failure.name || 'anonymous'}]`,
    };
  }
  return {
    name: THROWN_VALUE,
    message: stableJson(redact(failure, { keys: redactKeys })),
  };
}

// A V8 stack starts with name and message. replacedErrorInfo rebuilds that
// start from message and keeps only the frame lines of the original stack.
function replacedErrorInfo(failure: Error, message: string): ErrorInfo {
  const info: ErrorInfo = { name: failure.name, message };
  if (failure.stack !== undefined) {
    const frames = stackFrames(failure.stack, failure);
    info.stack = [`${failure.name}: ${message}`, ...frames].join('\n');
  }
  return info;
}

// A line of the message can look like a frame. Node 24 writes the stack text at
// the first read of err.stack, with the message of that moment. An assignment to
// err.message after that read leaves an older message of unknown length in the
// stack. stackFrames keeps no line of a stack whose start it cannot match to the
// current message. A new message equal to the leading lines of the old one does
// match, and a later old line that reads like a frame stays, as docs/log-format.md
// states.
function stackFrames(stack: string, failure: Error): string[] {
  const headerEnd = stackHeaderEnd(stack, failure.name, failure.message);
  if (headerEnd === undefined) {
    return [];
  }
  return stack
    .slice(headerEnd)
    .split('\n')
    .filter((line) => /^\s+at /.test(line));
}

// stackHeaderEnd returns the index after `{name}: {message}`. A stack that starts
// with err.name holds that name, and the name can contain ": " itself. Code can
// assign err.name after the stack text is written. The name in the stack then
// ends at the first ": ". V8 writes an empty message as the name alone and an
// empty name as the message alone. A source map handler can write both with the
// ": " in place. A first line without ": " that is neither the name nor the
// message can be a line of an old message, and the lines below it can be too.
function stackHeaderEnd(
  stack: string,
  name: string,
  message: string,
): number | undefined {
  const namePrefix = `${name}: `;
  if (stack.startsWith(namePrefix)) {
    return lineEndAfter(stack, namePrefix.length, message);
  }
  const loneHeader = loneHeaderEnd(stack, name, message);
  if (loneHeader !== undefined) {
    return loneHeader;
  }
  const newline = stack.indexOf('\n');
  const firstLineEnd = newline === -1 ? stack.length : newline;
  const nameEnd = stack.indexOf(': ');
  if (nameEnd === -1 || nameEnd > firstLineEnd) {
    return undefined;
  }
  return lineEndAfter(stack, nameEnd + 2, message);
}

function loneHeaderEnd(
  stack: string,
  name: string,
  message: string,
): number | undefined {
  if (message === '') {
    return lineEndAfter(stack, 0, name);
  }
  return name === '' ? lineEndAfter(stack, 0, message) : undefined;
}

// lineEndAfter returns the index after text at from when a line ends there.
function lineEndAfter(
  stack: string,
  from: number,
  text: string,
): number | undefined {
  const end = from + text.length;
  const endsLine = end === stack.length || stack.charAt(end) === '\n';
  return stack.startsWith(text, from) && endsLine ? end : undefined;
}

// An Error created in another realm, such as a vm context, fails instanceof
// Error. Error.isError and util.types.isNativeError both recognise it. Node docs
// deprecate util.types.isNativeError since 24.2.0. It serves runtimes that lack
// Error.isError.
const runtimeIsError: (value: unknown) => boolean =
  (Error as { isError?: (value: unknown) => boolean }).isError ??
  types.isNativeError;

function isError(value: object): value is Error {
  return value instanceof Error || runtimeIsError(value);
}

/**
 * markReported marks failure as reported in request. It returns true when
 * failure was not marked there before.
 */
export function markReported(request: RequestScope, failure: object): boolean {
  if (request.reported.has(failure)) {
    return false;
  }
  request.reported.add(failure);
  return true;
}
