export const REDACTED = '[REDACTED]';
export const CIRCULAR = '[CIRCULAR]';
export const TRUNCATED = '[TRUNCATED]';

export const DEFAULT_REDACT_KEYS = [
  'password',
  'token',
  'accessToken',
  'refreshToken',
  'apiKey',
  'secret',
  'authorization',
  'cookie',
  'email',
  'phone',
];

const DEFAULT_MAX_DEPTH = 8;

export interface RedactOptions {
  /** Keys masked in addition to DEFAULT_REDACT_KEYS. */
  keys?: string[];
  /**
   * Nesting level below which values are replaced by TRUNCATED. Anything
   * other than a non-negative integer falls back to the default of 8.
   */
  maxDepth?: number;
}

/**
 * redact returns a copy of value with every property whose key matches a
 * sensitive key replaced by REDACTED. Keys match without regard to case, at
 * any depth, inside objects and arrays. The input is never modified. Only
 * keys are matched. Sensitive text inside a string value, such as an error
 * message, is left as it is.
 */
export function redact(value: unknown, opts: RedactOptions = {}): unknown {
  const keys = new Set(
    [...DEFAULT_REDACT_KEYS, ...(opts.keys ?? [])].map((key) =>
      key.toLowerCase(),
    ),
  );
  const ancestors = new WeakSet<object>();
  return redactValue(value, keys, maxDepthOf(opts.maxDepth), ancestors);
}

function maxDepthOf(requested: number | undefined): number {
  if (
    requested !== undefined &&
    Number.isInteger(requested) &&
    requested >= 0
  ) {
    return requested;
  }
  return DEFAULT_MAX_DEPTH;
}

// ancestors holds the objects on the path from the root to the current
// value. A value already on that path is a cycle. The same object reached
// through two sibling properties is copied twice.
function redactValue(
  value: unknown,
  keys: Set<string>,
  depthLeft: number,
  ancestors: WeakSet<object>,
): unknown {
  if (value === null || typeof value !== 'object' || value instanceof Date) {
    return value;
  }
  if (ancestors.has(value)) {
    return CIRCULAR;
  }
  if (depthLeft === 0) {
    return TRUNCATED;
  }

  ancestors.add(value);
  try {
    if (Array.isArray(value)) {
      return value.map((item) =>
        redactValue(item, keys, depthLeft - 1, ancestors),
      );
    }
    const copy: Record<string, unknown> = {};
    for (const [key, inner] of Object.entries(value)) {
      copy[key] = keys.has(key.toLowerCase())
        ? REDACTED
        : redactValue(inner, keys, depthLeft - 1, ancestors);
    }
    return copy;
  } finally {
    ancestors.delete(value);
  }
}
