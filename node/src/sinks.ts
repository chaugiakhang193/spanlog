import type { Rendered } from './types.js';

/** SpanSink receives every rendered line that passes the level filter. */
export interface SpanSink {
  write(line: Rendered): void;
}

export type TextWriter = (text: string) => void;

function writeStdout(text: string): void {
  process.stdout.write(text);
}

/** prettySink writes the readable lines, one event per write. */
export function prettySink(write: TextWriter = writeStdout): SpanSink {
  return {
    write(line) {
      write(`${line.lines.join('\n')}\n`);
    },
  };
}

/**
 * jsonSink writes one JSON object per line: time in UTC, level, the fields of
 * docs/log-format.md, then msg.
 */
export function jsonSink(write: TextWriter = writeStdout): SpanSink {
  return {
    write(line) {
      const record = {
        time: line.time.toISOString(),
        level: line.level,
        ...line.fields,
        msg: line.msg,
      };
      write(`${JSON.stringify(record, bigintAsString)}\n`);
    },
  };
}

function bigintAsString(_key: string, value: unknown): unknown {
  return typeof value === 'bigint' ? value.toString() : value;
}

/** PinoLike is the part of a pino logger that pinoSink calls. */
export interface PinoLike {
  debug(obj: object, msg: string): void;
  info(obj: object, msg: string): void;
  warn(obj: object, msg: string): void;
  error(obj: object, msg: string): void;
}

/**
 * pinoSink hands the fields and the message to a pino logger. Time, level and
 * message encoding then follow the pino configuration.
 */
export function pinoSink(logger: PinoLike): SpanSink {
  return {
    write(line) {
      logger[line.level](line.fields, line.msg);
    },
  };
}
