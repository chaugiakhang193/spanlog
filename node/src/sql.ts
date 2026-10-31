/** SqlSummary is what a query line keeps of a SQL statement. */
export interface SqlSummary {
  op: string;
  tables: string[];
}

interface Token {
  /** Identifier text without quotes, or the punctuation character. */
  text: string;
  kind: 'word' | 'quoted' | 'punct';
}

const TABLE_KEYWORDS = new Set(['FROM', 'JOIN', 'INTO', 'UPDATE']);

// FOR UPDATE and FOR NO KEY UPDATE lock rows. ON CONFLICT ... DO UPDATE updates
// the row of the INSERT. None of them is followed by a table name.
// The UPDATE action of MERGE follows THEN and updates the row of the MERGE.
const UPDATE_WITHOUT_TABLE = new Set(['FOR', 'KEY', 'DO', 'THEN']);

// A common table expression holds one of these statements. A column list after
// WITH ORDINALITY AS materialized has the same shape up to its first name.
const STATEMENT_KEYWORDS = new Set([
  'SELECT',
  'VALUES',
  'TABLE',
  'WITH',
  'INSERT',
  'UPDATE',
  'DELETE',
  'MERGE',
]);

// Postgres accepts letters with diacritical marks and non-Latin letters in a
// name and in a dollar quote tag. Every character from U+0080 up counts here.
const DOLLAR_QUOTE_TAG =
  /\$(?:[A-Za-z_\u0080-\uFFFF][A-Za-z0-9_\u0080-\uFFFF]*)?\$/y;

/** CteScope is the range of tokens in which name refers to an expression. */
interface CteScope {
  name: string;
  from: number;
  to: number;
}

/**
 * summarizeSql reads the op and the table names of a statement, following
 * the query line rules of docs/log-format.md. It never throws.
 */
export function summarizeSql(sql: string): SqlSummary {
  const tokens = tokenize(sql);
  const afterClose = closingParentheses(tokens);
  const scopes: CteScope[] = [];
  const afterExpressions = new Set<number>();
  let statementStart = afterLeadingParentheses(tokens, 0);
  const openParentheses: number[] = [];
  tokens.forEach((token, index) => {
    if (isPunct(token, '(')) {
      openParentheses.push(index);
    } else if (isPunct(token, ')')) {
      openParentheses.pop();
    } else if (isKeyword(token, 'WITH')) {
      const innermost = openParentheses.at(-1);
      const scopeEnd =
        innermost === undefined ? undefined : afterClose[innermost];
      const after = readCommonTableExpressions(
        tokens,
        index,
        scopeEnd ?? tokens.length,
        afterClose,
        scopes,
      );
      afterExpressions.add(after);
      if (index === statementStart) {
        statementStart = afterLeadingParentheses(tokens, after);
      }
    }
  });
  return {
    op: opOf(tokens, statementStart),
    tables: tableNames(tokens, scopes, afterExpressions),
  };
}

function afterLeadingParentheses(tokens: Token[], from: number): number {
  let index = from;
  while (isPunct(tokens[index], '(')) {
    index += 1;
  }
  return index;
}

/** queryTarget joins op and tables into the target of a query line. */
export function queryTarget(summary: SqlSummary): string {
  if (summary.tables.length === 0) {
    return summary.op;
  }
  return `${summary.op} ${summary.tables.join(', ')}`;
}

// tokenize keeps words, double-quoted identifiers and the punctuation that
// shapes table names and subqueries. String literals and comments are dropped
// unread because they can hold application values. Everything else, such as
// $1 or a comparison operator, is dropped too.
function tokenize(sql: string): Token[] {
  const tokens: Token[] = [];
  let index = 0;
  while (index < sql.length) {
    const char = sql.charAt(index);
    if (isWordStart(char)) {
      const end = scanWhile(sql, index, isWordPart);
      const word = sql.slice(index, end);
      if (sql.charAt(end) === "'" && upperAscii(word) === 'E') {
        index = afterEscapeString(sql, end);
      } else {
        tokens.push({ text: word, kind: 'word' });
        index = end;
      }
    } else if (char === "'") {
      index = afterStringLiteral(sql, index, false);
    } else if (sql.startsWith('--', index)) {
      index = afterLineComment(sql, index);
    } else if (sql.startsWith('/*', index)) {
      index = afterBlockComment(sql, index);
    } else if (char === '"') {
      const [text, end] = scanQuoted(sql, index);
      tokens.push({ text, kind: 'quoted' });
      index = end;
    } else if (char === '(' || char === ')' || char === ',' || char === '.') {
      tokens.push({ text: char, kind: 'punct' });
      index += 1;
    } else if (char === '$') {
      index = afterDollarToken(sql, index);
    } else if (isDigit(char)) {
      // 42 must not leave a trailing word behind.
      index = scanWhile(sql, index + 1, isWordPart);
    } else {
      index += 1;
    }
  }
  return tokens;
}

function scanWhile(
  sql: string,
  from: number,
  accept: (char: string) => boolean,
): number {
  let end = from;
  while (end < sql.length && accept(sql.charAt(end))) {
    end += 1;
  }
  return end;
}

// A doubled quote inside a quoted identifier stands for one quote character.
function scanQuoted(sql: string, from: number): [string, number] {
  let text = '';
  let index = from + 1;
  while (index < sql.length) {
    const char = sql.charAt(index);
    if (char === '"') {
      if (sql.charAt(index + 1) !== '"') {
        return [text, index + 1];
      }
      index += 1;
    }
    text += char;
    index += 1;
  }
  return [text, index];
}

// A doubled quote stands for one quote character. A backslash escapes the next
// character only in an E'...' string. Postgres keeps it literal elsewhere.
function afterStringLiteral(
  sql: string,
  open: number,
  backslashEscapes: boolean,
): number {
  let index = open + 1;
  while (index < sql.length) {
    const char = sql.charAt(index);
    if (backslashEscapes && char === '\\') {
      index += 2;
    } else if (char !== "'") {
      index += 1;
    } else if (sql.charAt(index + 1) === "'") {
      index += 2;
    } else {
      return index + 1;
    }
  }
  return sql.length;
}

// afterEscapeString skips an E'...' string and its continuations. Only the
// first part carries the E, and every part reads a backslash as an escape.
function afterEscapeString(sql: string, open: number): number {
  let end = afterStringLiteral(sql, open, true);
  let next = continuationQuote(sql, end);
  while (next !== undefined) {
    end = afterStringLiteral(sql, next, true);
    next = continuationQuote(sql, end);
  }
  return end;
}

// continuationQuote returns the index of the quote that continues the string
// ending at from. Whitespace and line comments may stand between the two parts.
// Postgres rejects two parts with no newline between them.
function continuationQuote(sql: string, from: number): number | undefined {
  let index = from;
  for (;;) {
    const char = sql.charAt(index);
    if (
      isNewline(char) ||
      char === ' ' ||
      char === '\t' ||
      char === '\f' ||
      char === '\v'
    ) {
      index += 1;
    } else if (sql.startsWith('--', index)) {
      index = afterLineComment(sql, index);
    } else {
      return char === "'" ? index : undefined;
    }
  }
}

function isNewline(char: string): boolean {
  return char === '\n' || char === '\r';
}

// Postgres ends a line comment at a carriage return as well as a line feed.
function afterLineComment(sql: string, open: number): number {
  return scanWhile(sql, open, (char) => !isNewline(char));
}

// Postgres block comments nest.
function afterBlockComment(sql: string, open: number): number {
  let depth = 0;
  let index = open;
  while (index < sql.length) {
    if (sql.startsWith('/*', index)) {
      depth += 1;
      index += 2;
    } else if (sql.startsWith('*/', index)) {
      depth -= 1;
      index += 2;
      if (depth === 0) {
        return index;
      }
    } else {
      index += 1;
    }
  }
  return sql.length;
}

// afterDollarToken skips a $tag$...$tag$ string or a parameter such as $1.
function afterDollarToken(sql: string, open: number): number {
  DOLLAR_QUOTE_TAG.lastIndex = open;
  const tag = DOLLAR_QUOTE_TAG.exec(sql)?.[0];
  if (tag === undefined) {
    return scanWhile(sql, open + 1, isWordPart);
  }
  const close = sql.indexOf(tag, open + tag.length);
  return close === -1 ? sql.length : close + tag.length;
}

function isWordStart(char: string): boolean {
  return /[A-Za-z_\u0080-\uFFFF]/.test(char);
}

function isWordPart(char: string): boolean {
  return /[A-Za-z0-9_$\u0080-\uFFFF]/.test(char);
}

function isDigit(char: string): boolean {
  return char >= '0' && char <= '9';
}

// Postgres folds only ASCII letters when it looks up a keyword. toUpperCase
// turns the letters ı and ß of a name into I and SS.
function upperAscii(text: string): string {
  return text.replace(/[a-z]+/g, (letters) => letters.toUpperCase());
}

function isKeyword(token: Token | undefined, keyword: string): boolean {
  return token?.kind === 'word' && upperAscii(token.text) === keyword;
}

function isPunct(token: Token | undefined, char: string): boolean {
  return token?.kind === 'punct' && token.text === char;
}

// Postgres keeps a quoted name as is. In a UTF-8 database it folds A-Z of an
// unquoted name and keeps every other letter. É and é are two names there.
function foldedName(token: Token): string {
  if (token.kind === 'quoted') {
    return token.text;
  }
  return token.text.replace(/[A-Z]+/g, (letters) => letters.toLowerCase());
}

// closingParentheses gives each "(" the index after its closing ")", or the
// token count when it never closes. Only "(" positions are set.
function closingParentheses(tokens: Token[]): number[] {
  const afterClose: number[] = [];
  const open: number[] = [];
  tokens.forEach((token, index) => {
    if (isPunct(token, '(')) {
      open.push(index);
      afterClose[index] = tokens.length;
    } else if (isPunct(token, ')')) {
      const opener = open.pop();
      if (opener !== undefined) {
        afterClose[opener] = index + 1;
      }
    }
  });
  return afterClose;
}

// readCommonTableExpressions walks WITH [RECURSIVE] name [(columns)] AS
// [NOT] [MATERIALIZED] (...) [SEARCH ...] [CYCLE ...], ... from the WITH at
// withIndex and returns the index of the keyword after the last expression.
// Each name goes into scopes up to scopeEnd, the end of the statement or
// subquery that holds the WITH. A plain expression becomes visible after its
// own body. Inside that body its name still reads a table. RECURSIVE makes every
// name of the list visible from the WITH on. A name without a statement in
// parentheses is left out, as in WITH TIME ZONE and WITH ORDINALITY AS t.
function readCommonTableExpressions(
  tokens: Token[],
  withIndex: number,
  scopeEnd: number,
  afterClose: number[],
  scopes: CteScope[],
): number {
  const recursive = isKeyword(tokens[withIndex + 1], 'RECURSIVE');
  let index = withIndex + (recursive ? 2 : 1);
  for (;;) {
    const name = tokens[index];
    if (name === undefined || name.kind === 'punct') {
      return index;
    }
    index = isPunct(tokens[index + 1], '(')
      ? (afterClose[index + 1] ?? tokens.length)
      : index + 1;
    while (
      isKeyword(tokens[index], 'AS') ||
      isKeyword(tokens[index], 'NOT') ||
      isKeyword(tokens[index], 'MATERIALIZED')
    ) {
      index += 1;
    }
    if (!isPunct(tokens[index], '(') || !opensStatement(tokens[index + 1])) {
      return index;
    }
    index = afterClose[index] ?? tokens.length;
    index = afterClause(tokens, index, 'SEARCH', 'SET');
    index = afterClause(tokens, index, 'CYCLE', 'USING');
    scopes.push({
      name: foldedName(name),
      from: recursive ? withIndex : index,
      to: scopeEnd,
    });
    if (!isPunct(tokens[index], ',')) {
      return index;
    }
    index += 1;
  }
}

function opensStatement(token: Token | undefined): boolean {
  if (isPunct(token, '(')) {
    return true;
  }
  return (
    token?.kind === 'word' && STATEMENT_KEYWORDS.has(upperAscii(token.text))
  );
}

// afterClause skips a SEARCH or CYCLE clause of a common table expression. Each
// ends with its last keyword and one column name. A clause holds no parenthesis
// in the forms it reads, and stopping at one keeps text without lastKeyword from
// being scanned again by every WITH.
function afterClause(
  tokens: Token[],
  from: number,
  firstKeyword: string,
  lastKeyword: string,
): number {
  if (!isKeyword(tokens[from], firstKeyword)) {
    return from;
  }
  let index = from + 1;
  while (
    index < tokens.length &&
    !isKeyword(tokens[index], lastKeyword) &&
    !isPunct(tokens[index], '(')
  ) {
    index += 1;
  }
  return isKeyword(tokens[index], lastKeyword) ? index + 2 : index;
}

// scopeChanges lists the scopes that open (+1) or close (-1) at each token.
// tableNames walks the tokens once and keeps a count per name. Many scopes of
// one name then cost no scan at each table keyword.
function scopeChanges(scopes: CteScope[]): Map<number, [string, number][]> {
  const changes = new Map<number, [string, number][]>();
  const addChange = (index: number, name: string, delta: number): void => {
    const atIndex = changes.get(index);
    if (atIndex === undefined) {
      changes.set(index, [[name, delta]]);
    } else {
      atIndex.push([name, delta]);
    }
  };
  for (const scope of scopes) {
    addChange(scope.from, scope.name, 1);
    addChange(scope.to, scope.name, -1);
  }
  return changes;
}

function opOf(tokens: Token[], index: number): string {
  const first = tokens[index];
  if (first === undefined || first.kind !== 'word') {
    return '';
  }
  const op = upperAscii(first.text);
  if (op === 'START' && isKeyword(tokens[index + 1], 'TRANSACTION')) {
    return 'START TRANSACTION';
  }
  return op;
}

function tableNames(
  tokens: Token[],
  scopes: CteScope[],
  afterExpressions: Set<number>,
): string[] {
  const tables = new Set<string>();
  const changes = scopeChanges(scopes);
  const visibleScopes = new Map<string, number>();
  for (let index = 0; index < tokens.length; index += 1) {
    for (const [name, delta] of changes.get(index) ?? []) {
      visibleScopes.set(name, (visibleScopes.get(name) ?? 0) + delta);
    }
    if (!isTableKeyword(tokens, index)) {
      continue;
    }
    const parts = qualifiedName(tokens, index + 1);
    const [first] = parts;
    if (first === undefined) {
      continue;
    }
    if (
      parts.length === 1 &&
      !isWriteTarget(tokens, index, afterExpressions) &&
      (visibleScopes.get(foldedName(first)) ?? 0) > 0
    ) {
      continue;
    }
    tables.add(parts.map((part) => part.text).join('.'));
  }
  return [...tables];
}

// Postgres looks up the table of DELETE FROM, INSERT INTO, MERGE INTO and UPDATE
// without looking at common table expressions.
function isWriteTarget(
  tokens: Token[],
  keywordIndex: number,
  afterExpressions: Set<number>,
): boolean {
  const keyword = tokens[keywordIndex];
  if (isKeyword(keyword, 'FROM')) {
    return isDeleteCommand(tokens, keywordIndex - 1, afterExpressions);
  }
  return !isKeyword(keyword, 'JOIN');
}

// isDeleteCommand tells the DELETE command from a column named delete. With a
// common table expression in sight, the command opens the body of an expression
// or follows the list of expressions.
function isDeleteCommand(
  tokens: Token[],
  index: number,
  afterExpressions: Set<number>,
): boolean {
  return (
    isKeyword(tokens[index], 'DELETE') &&
    (isPunct(tokens[index - 1], '(') || afterExpressions.has(index))
  );
}

function isTableKeyword(tokens: Token[], index: number): boolean {
  const token = tokens[index];
  if (token?.kind !== 'word') {
    return false;
  }
  const keyword = upperAscii(token.text);
  if (!TABLE_KEYWORDS.has(keyword)) {
    return false;
  }
  if (keyword === 'FROM') {
    return !isDistinctFromOperator(tokens, index);
  }
  if (keyword !== 'UPDATE') {
    return true;
  }
  const previous = tokens[index - 1];
  return !(
    previous?.kind === 'word' &&
    UPDATE_WITHOUT_TABLE.has(upperAscii(previous.text))
  );
}

// a IS [NOT] DISTINCT FROM b compares two values.
function isDistinctFromOperator(tokens: Token[], fromIndex: number): boolean {
  return (
    isKeyword(tokens[fromIndex - 1], 'DISTINCT') &&
    (isKeyword(tokens[fromIndex - 2], 'IS') ||
      isKeyword(tokens[fromIndex - 2], 'NOT'))
  );
}

// firstNameIndex skips what can sit between a table keyword and the name it
// introduces: LATERAL, ONLY, and the parenthesis of ONLY (name).
function firstNameIndex(tokens: Token[], index: number): number {
  if (isKeyword(tokens[index], 'LATERAL')) {
    return index + 1;
  }
  if (!isKeyword(tokens[index], 'ONLY')) {
    return index;
  }
  return isPunct(tokens[index + 1], '(') ? index + 2 : index + 1;
}

// qualifiedName reads name or schema.name starting at index. A subquery,
// which starts with a parenthesis, has no name.
function qualifiedName(tokens: Token[], index: number): Token[] {
  const parts: Token[] = [];
  let position = firstNameIndex(tokens, index);
  for (;;) {
    const part = tokens[position];
    if (part === undefined || part.kind === 'punct') {
      break;
    }
    parts.push(part);
    if (!isPunct(tokens[position + 1], '.')) {
      break;
    }
    position += 2;
  }
  return parts;
}
