import { describe, expect, it } from 'vitest';
import { withoutOwnFrames } from './frames.js';

const ALS_RUN =
  'at AsyncLocalStorage.run (node:internal/async_local_storage/async_context_frame:63:14)';

describe('withoutOwnFrames', () => {
  it('drops spanlog frames in every form V8 prints', () => {
    expect(
      withoutOwnFrames([
        'at Inner.fail (file:///app/inner.js:3:11)',
        'at spanlogInvoke (file:///app/node_modules/spanlog/dist/spanlog.js:1:1)',
        'at async spanlogTrack (file:///app/node_modules/spanlog/dist/spanlog.js:2:2)',
        'at Inner.spanlogWrapped (C:\\app\\node_modules\\spanlog\\src\\spanlog.ts:3:3)',
        'at async Outer.run (file:///app/outer.js:9:5)',
      ]),
    ).toEqual([
      'at Inner.fail (file:///app/inner.js:3:11)',
      'at async Outer.run (file:///app/outer.js:9:5)',
    ]);
  });

  it('drops AsyncLocalStorage.run only when a spanlog frame follows it', () => {
    expect(
      withoutOwnFrames([
        'at spanlogEnter (x.js:1:1)',
        ALS_RUN,
        'at spanlogRunInContext (x.js:2:2)',
        'at handler (file:///app/h.js:1:1)',
        ALS_RUN,
        'at middleware (file:///app/m.js:1:1)',
      ]),
    ).toEqual([
      'at handler (file:///app/h.js:1:1)',
      ALS_RUN,
      'at middleware (file:///app/m.js:1:1)',
    ]);
  });

  it('keeps frames whose names only look alike', () => {
    const lines = [
      'at track (file:///app/t.js:1:1)',
      'at Script.runInContext (node:vm:1:1)',
      'at spanlogger (file:///app/l.js:1:1)',
      'at mySpanlogThing (file:///app/x.js:1:1)',
      'at Parser.spanlogParse (file:///app/p.js:1:1)',
      'at async spanlogFetch (file:///app/f.js:1:1)',
    ];

    expect(withoutOwnFrames(lines)).toEqual(lines);
  });
});
