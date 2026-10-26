export type Level = 'debug' | 'info' | 'warn' | 'error';

export type Action = 'request' | 'run' | 'call' | 'query';

export type Phase = 'START' | 'DONE' | 'SLOW' | 'FAILED' | 'ABORTED';

export interface ErrorInfo {
  name?: string;
  message: string;
  stack?: string;
}

/**
 * SpanEvent is one line before rendering. The field list and the meaning of
 * each field follow docs/log-format.md.
 */
export interface SpanEvent {
  time: Date;
  service: string;
  reqId?: string;
  /** Scope of the frame that owns the line. */
  scope?: string;
  /** Method of the frame that owns the line. */
  method?: string;
  action: Action;
  target?: string;
  /** True for a call whose callee has the same scope as the caller. */
  sameScope?: boolean;
  phase: Phase;
  /** Raw duration. Absent on START. */
  durationMs?: number;
  status?: number;
  op?: string;
  tables?: string[];
  err?: ErrorInfo;
  /** True when this line is the first report of err in the request. */
  firstReport?: boolean;
  params?: Record<string, unknown>;
  summary?: Record<string, unknown>;
}

export interface Rendered {
  time: Date;
  level: Level;
  msg: string;
  fields: Record<string, unknown>;
  /** The readable line, followed by stack lines on the first report of an error. */
  lines: string[];
}
