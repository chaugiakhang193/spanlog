// OWN_FUNCTIONS lists the functions of spanlog that can sit on the stack of an
// application error. V8 prints such a frame as "at spanlogTrack (...)", "at
// async spanlogTrack (...)" or, with the receiver in front, "at
// Inner.spanlogWrapped (...)". The name survives source maps and test runners,
// which rewrite the file path. Only these exact names match. An application
// function named spanlogParse keeps its frame. One named spanlogTrack loses it.
const OWN_FUNCTIONS = [
  'spanlogCall',
  'spanlogEnter',
  'spanlogEnterRequest',
  'spanlogInvoke',
  'spanlogMiddleware',
  'spanlogRunInContext',
  'spanlogTrack',
  'spanlogWithRequest',
  'spanlogWrapped',
];

const OWN_FRAME = new RegExp(
  `^at (?:async )?(?:[^\\s(]+\\.)?(?:${OWN_FUNCTIONS.join('|')})(?:\\s|$)`,
);

const ASYNC_LOCAL_STORAGE_RUN = /^at AsyncLocalStorage\.run \(node:internal\//;

/**
 * withoutOwnFrames drops trimmed stack lines that belong to spanlog: frames of
 * its named functions, and an AsyncLocalStorage.run frame directly called by
 * one of them. The application's own AsyncLocalStorage.run frames stay.
 */
export function withoutOwnFrames(lines: string[]): string[] {
  return lines.filter((line, index) => {
    if (OWN_FRAME.test(line)) {
      return false;
    }
    if (ASYNC_LOCAL_STORAGE_RUN.test(line)) {
      const caller = lines[index + 1];
      return caller === undefined || !OWN_FRAME.test(caller);
    }
    return true;
  });
}
