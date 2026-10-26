import { describe, expect, it } from 'vitest';
import {
  CIRCULAR,
  DEFAULT_REDACT_KEYS,
  REDACTED,
  redact,
  TRUNCATED,
} from './redact.js';

describe('redact', () => {
  it('masks every default key', () => {
    const input = Object.fromEntries(
      DEFAULT_REDACT_KEYS.map((key) => [key, 'secret value']),
    );

    expect(Object.values(redact(input) as object)).toEqual(
      DEFAULT_REDACT_KEYS.map(() => REDACTED),
    );
    expect(DEFAULT_REDACT_KEYS).toEqual([
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
    ]);
  });

  it('masks default keys at any depth without regard to case', () => {
    const input = {
      Password: 'p',
      user: { email: 'a@example.com', name: 'An' },
      headers: [{ Authorization: 'Bearer x' }],
    };

    expect(redact(input)).toEqual({
      Password: REDACTED,
      user: { email: REDACTED, name: 'An' },
      headers: [{ Authorization: REDACTED }],
    });
  });

  it('adds extra keys to the default list', () => {
    expect(
      redact({ nationalId: '1', token: 't' }, { keys: ['nationalId'] }),
    ).toEqual({
      nationalId: REDACTED,
      token: REDACTED,
    });
  });

  it('returns a copy and leaves the input untouched', () => {
    const input = { password: 'p', nested: { secret: 's' } };

    redact(input);

    expect(input).toEqual({ password: 'p', nested: { secret: 's' } });
  });

  it('replaces a cycle and keeps an object shared by two properties', () => {
    const shared = { sku: 'A1' };
    const input: Record<string, unknown> = { first: shared, second: shared };
    input.self = input;

    expect(redact(input)).toEqual({
      first: { sku: 'A1' },
      second: { sku: 'A1' },
      self: CIRCULAR,
    });
  });

  it('truncates values nested deeper than maxDepth', () => {
    expect(redact({ a: { b: { c: 1 } } }, { maxDepth: 2 })).toEqual({
      a: { b: TRUNCATED },
    });
  });

  const nineLevels = {
    l1: { l2: { l3: { l4: { l5: { l6: { l7: { l8: { l9: 1 } } } } } } } },
  };
  const cutAtNinth = {
    l1: { l2: { l3: { l4: { l5: { l6: { l7: { l8: TRUNCATED } } } } } } },
  };

  it('keeps 8 levels by default and cuts the ninth', () => {
    expect(redact(nineLevels)).toEqual(cutAtNinth);
  });

  it('falls back to the default depth for an invalid maxDepth', () => {
    for (const maxDepth of [-1, Number.NaN, 2.5]) {
      expect(redact(nineLevels, { maxDepth })).toEqual(cutAtNinth);
    }
  });

  it('truncates the root object at maxDepth 0 and keeps a root primitive', () => {
    expect(redact({ a: 1 }, { maxDepth: 0 })).toBe(TRUNCATED);
    expect(redact([1], { maxDepth: 0 })).toBe(TRUNCATED);
    expect(redact(5, { maxDepth: 0 })).toBe(5);
  });

  it('passes primitives and dates through', () => {
    const at = new Date('2026-01-01T00:00:00Z');

    expect(redact('text')).toBe('text');
    expect(redact(null)).toBeNull();
    expect(redact({ at })).toEqual({ at });
  });
});
