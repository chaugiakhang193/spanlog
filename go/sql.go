package spanlog

import "strings"

// sqlSummary is what a query line keeps of a SQL statement.
type sqlSummary struct {
	Op     string
	Tables []string
}

type tokenKind int

const (
	tokenWord tokenKind = iota
	tokenQuoted
	tokenPunct
)

type sqlToken struct {
	// text is the identifier without quotes, or the punctuation character.
	text string
	kind tokenKind
}

var tableKeywords = map[string]bool{"FROM": true, "JOIN": true, "INTO": true, "UPDATE": true}

// FOR UPDATE and FOR NO KEY UPDATE lock rows. ON CONFLICT ... DO UPDATE updates
// the row of the INSERT. None of them is followed by a table name. The UPDATE
// action of MERGE follows THEN and updates the row of the MERGE.
var updateWithoutTable = map[string]bool{"FOR": true, "KEY": true, "DO": true, "THEN": true}

// A common table expression holds one of these statements. A column list after
// WITH ORDINALITY AS materialized has the same shape up to its first name.
var statementKeywords = map[string]bool{
	"SELECT": true, "VALUES": true, "TABLE": true, "WITH": true,
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true,
}

// cteScope is the range of tokens in which name refers to an expression.
type cteScope struct {
	name     string
	from, to int
}

// summarizeSQL reads the op and the table names of a statement, following the
// query line rules of docs/log-format.md. It never panics.
func summarizeSQL(sql string) sqlSummary {
	tokens := tokenize(sql)
	afterClose := closingParentheses(tokens)
	var scopes []cteScope
	afterExpressions := map[int]bool{}
	statementStart := afterLeadingParentheses(tokens, 0)
	var openParentheses []int
	for index, token := range tokens {
		switch {
		case isPunct(token, '('):
			openParentheses = append(openParentheses, index)
		case isPunct(token, ')'):
			if len(openParentheses) > 0 {
				openParentheses = openParentheses[:len(openParentheses)-1]
			}
		case isKeyword(token, "WITH"):
			scopeEnd := len(tokens)
			if len(openParentheses) > 0 {
				scopeEnd = afterClose[openParentheses[len(openParentheses)-1]]
			}
			after := readCommonTableExpressions(tokens, index, scopeEnd, afterClose, &scopes)
			afterExpressions[after] = true
			if index == statementStart {
				statementStart = afterLeadingParentheses(tokens, after)
			}
		}
	}
	return sqlSummary{
		Op:     opOf(tokens, statementStart),
		Tables: tableNames(tokens, scopes, afterExpressions),
	}
}

func afterLeadingParentheses(tokens []sqlToken, from int) int {
	index := from
	for index < len(tokens) && isPunct(tokens[index], '(') {
		index++
	}
	return index
}

// queryTarget joins op and tables into the target of a query line.
func queryTarget(summary sqlSummary) string {
	if len(summary.Tables) == 0 {
		return summary.Op
	}
	return summary.Op + " " + strings.Join(summary.Tables, ", ")
}

// rollsBackToSavepoint tells ROLLBACK TO SAVEPOINT apart from the ROLLBACK of a
// whole transaction. summarizeSQL gives both the op ROLLBACK.
func rollsBackToSavepoint(sql string) bool {
	tokens := tokenize(sql)
	if !isKeyword(tokenAt(tokens, 0), "ROLLBACK") {
		return false
	}
	return isKeyword(tokenAt(tokens, afterWorkOrTransaction(tokens)), "TO")
}

var transactionEnds = []string{"COMMIT", "END", "ABORT", "ROLLBACK"}

// chainsTransaction tells COMMIT, END, ABORT or ROLLBACK with AND CHAIN apart
// from the same statement without it. Postgres starts the next transaction once
// such a statement succeeds.
func chainsTransaction(sql string) bool {
	tokens := tokenize(sql)
	first := tokenAt(tokens, 0)
	ends := false
	for _, word := range transactionEnds {
		if isKeyword(first, word) {
			ends = true
			break
		}
	}
	if !ends {
		return false
	}
	afterWord := afterWorkOrTransaction(tokens)
	return isKeyword(tokenAt(tokens, afterWord), "AND") && isKeyword(tokenAt(tokens, afterWord+1), "CHAIN")
}

// afterWorkOrTransaction returns the position after the optional WORK or
// TRANSACTION that follows the first keyword of a transaction statement.
func afterWorkOrTransaction(tokens []sqlToken) int {
	second := tokenAt(tokens, 1)
	if isKeyword(second, "WORK") || isKeyword(second, "TRANSACTION") {
		return 2
	}
	return 1
}

// tokenize keeps words, double-quoted identifiers and the punctuation that
// shapes table names and subqueries. String literals and comments are dropped
// unread because they can hold application values. Everything else, such as $1
// or a comparison operator, is dropped too. The scan works on bytes. Every byte
// of a multi-byte UTF-8 character is 0x80 or above and counts as a letter, as
// Postgres counts every character from U+0080 up.
func tokenize(sql string) []sqlToken {
	var tokens []sqlToken
	index := 0
	for index < len(sql) {
		char := sql[index]
		switch {
		case isWordStart(char):
			end := scanWhile(sql, index, isWordPart)
			word := sql[index:end]
			if byteAt(sql, end) == '\'' && (word == "E" || word == "e") {
				index = afterEscapeString(sql, end)
			} else {
				tokens = append(tokens, sqlToken{text: word, kind: tokenWord})
				index = end
			}
		case char == '\'':
			index = afterStringLiteral(sql, index, false)
		case strings.HasPrefix(sql[index:], "--"):
			index = afterLineComment(sql, index)
		case strings.HasPrefix(sql[index:], "/*"):
			index = afterBlockComment(sql, index)
		case char == '"':
			text, end := scanQuoted(sql, index)
			tokens = append(tokens, sqlToken{text: text, kind: tokenQuoted})
			index = end
		case char == '(' || char == ')' || char == ',' || char == '.':
			tokens = append(tokens, sqlToken{text: string(char), kind: tokenPunct})
			index++
		case char == '$':
			index = afterDollarToken(sql, index)
		case isDigit(char):
			// 42 must not leave a trailing word behind.
			index = scanWhile(sql, index+1, isWordPart)
		default:
			index++
		}
	}
	return tokens
}

// byteAt returns 0 past the end of sql. A NUL byte is not a character any rule
// looks for.
func byteAt(sql string, index int) byte {
	if index < 0 || index >= len(sql) {
		return 0
	}
	return sql[index]
}

func scanWhile(sql string, from int, accept func(byte) bool) int {
	end := from
	for end < len(sql) && accept(sql[end]) {
		end++
	}
	return end
}

// A doubled quote inside a quoted identifier stands for one quote character.
func scanQuoted(sql string, from int) (string, int) {
	var text strings.Builder
	index := from + 1
	for index < len(sql) {
		char := sql[index]
		if char == '"' {
			if byteAt(sql, index+1) != '"' {
				return text.String(), index + 1
			}
			index++
		}
		text.WriteByte(char)
		index++
	}
	return text.String(), index
}

// A doubled quote stands for one quote character. A backslash escapes the next
// character only in an E'...' string. Postgres keeps it literal elsewhere.
func afterStringLiteral(sql string, open int, backslashEscapes bool) int {
	index := open + 1
	for index < len(sql) {
		char := sql[index]
		switch {
		case backslashEscapes && char == '\\':
			index += 2
		case char != '\'':
			index++
		case byteAt(sql, index+1) == '\'':
			index += 2
		default:
			return index + 1
		}
	}
	return len(sql)
}

// afterEscapeString skips an E'...' string and its continuations. Only the
// first part carries the E, and every part reads a backslash as an escape.
func afterEscapeString(sql string, open int) int {
	end := afterStringLiteral(sql, open, true)
	for {
		next, found := continuationQuote(sql, end)
		if !found {
			return end
		}
		end = afterStringLiteral(sql, next, true)
	}
}

// continuationQuote returns the index of the quote that continues the string
// ending at from. Whitespace and line comments may stand between the two parts.
// Postgres rejects two parts with no newline between them.
func continuationQuote(sql string, from int) (int, bool) {
	index := from
	for {
		char := byteAt(sql, index)
		switch {
		case isNewline(char) || char == ' ' || char == '\t' || char == '\f' || char == '\v':
			index++
		case index < len(sql) && strings.HasPrefix(sql[index:], "--"):
			index = afterLineComment(sql, index)
		default:
			return index, char == '\''
		}
	}
}

func isNewline(char byte) bool {
	return char == '\n' || char == '\r'
}

// Postgres ends a line comment at a carriage return as well as a line feed.
func afterLineComment(sql string, open int) int {
	return scanWhile(sql, open, func(char byte) bool { return !isNewline(char) })
}

// Postgres block comments nest.
func afterBlockComment(sql string, open int) int {
	depth := 0
	index := open
	for index < len(sql) {
		switch {
		case strings.HasPrefix(sql[index:], "/*"):
			depth++
			index += 2
		case strings.HasPrefix(sql[index:], "*/"):
			depth--
			index += 2
			if depth == 0 {
				return index
			}
		default:
			index++
		}
	}
	return len(sql)
}

// afterDollarToken skips a $tag$...$tag$ string or a parameter such as $1. A
// tag starts with a letter or an underscore and holds letters, digits and
// underscores.
func afterDollarToken(sql string, open int) int {
	tagEnd := open + 1
	if tagEnd < len(sql) && isWordStart(sql[tagEnd]) {
		tagEnd = scanWhile(sql, tagEnd, isTagPart)
	}
	if byteAt(sql, tagEnd) != '$' {
		return scanWhile(sql, open+1, isWordPart)
	}
	tag := sql[open : tagEnd+1]
	close := strings.Index(sql[tagEnd+1:], tag)
	if close == -1 {
		return len(sql)
	}
	return tagEnd + 1 + close + len(tag)
}

func isLetter(char byte) bool {
	return (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char >= 0x80
}

func isDigit(char byte) bool {
	return char >= '0' && char <= '9'
}

func isWordStart(char byte) bool {
	return isLetter(char) || char == '_'
}

func isTagPart(char byte) bool {
	return isWordStart(char) || isDigit(char)
}

func isWordPart(char byte) bool {
	return isTagPart(char) || char == '$'
}

// upperASCII folds a-z only. Postgres folds only ASCII letters when it looks up
// a keyword, and strings.ToUpper turns the letters ı and ß of a name into I and
// SS.
func upperASCII(text string) string {
	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' {
			return char - 'a' + 'A'
		}
		return char
	}, text)
}

// tokenAt returns the zero token, which is no keyword and no punctuation,
// outside tokens.
func tokenAt(tokens []sqlToken, index int) sqlToken {
	if index < 0 || index >= len(tokens) {
		return sqlToken{kind: -1}
	}
	return tokens[index]
}

func isKeyword(token sqlToken, keyword string) bool {
	return token.kind == tokenWord && upperASCII(token.text) == keyword
}

func isPunct(token sqlToken, char byte) bool {
	return token.kind == tokenPunct && token.text[0] == char
}

// Postgres keeps a quoted name as is. In a UTF-8 database it folds A-Z of an
// unquoted name and keeps every other letter. É and é are two names there.
func foldedName(token sqlToken) string {
	if token.kind == tokenQuoted {
		return token.text
	}
	return strings.Map(func(char rune) rune {
		if char >= 'A' && char <= 'Z' {
			return char - 'A' + 'a'
		}
		return char
	}, token.text)
}

// closingParentheses gives each "(" the index after its closing ")", or the
// token count when it never closes. Only "(" positions are set.
func closingParentheses(tokens []sqlToken) []int {
	afterClose := make([]int, len(tokens))
	var open []int
	for index, token := range tokens {
		switch {
		case isPunct(token, '('):
			open = append(open, index)
			afterClose[index] = len(tokens)
		case isPunct(token, ')'):
			if len(open) > 0 {
				afterClose[open[len(open)-1]] = index + 1
				open = open[:len(open)-1]
			}
		}
	}
	return afterClose
}

// readCommonTableExpressions walks WITH [RECURSIVE] name [(columns)] AS [NOT]
// [MATERIALIZED] (...) [SEARCH ...] [CYCLE ...], ... from the WITH at withIndex
// and returns the index of the keyword after the last expression. Each name
// goes into scopes up to scopeEnd, the end of the statement or subquery that
// holds the WITH. A plain expression becomes visible after its own body. Inside
// that body its name still reads a table. RECURSIVE makes every name of the
// list visible from the WITH on. A name without a statement in parentheses is
// left out, as in WITH TIME ZONE and WITH ORDINALITY AS t.
func readCommonTableExpressions(tokens []sqlToken, withIndex, scopeEnd int, afterClose []int, scopes *[]cteScope) int {
	recursive := isKeyword(tokenAt(tokens, withIndex+1), "RECURSIVE")
	index := withIndex + 1
	if recursive {
		index = withIndex + 2
	}
	for {
		if index >= len(tokens) || tokens[index].kind == tokenPunct {
			return index
		}
		name := tokens[index]
		if isPunct(tokenAt(tokens, index+1), '(') {
			index = afterClose[index+1]
		} else {
			index++
		}
		for isKeyword(tokenAt(tokens, index), "AS") || isKeyword(tokenAt(tokens, index), "NOT") ||
			isKeyword(tokenAt(tokens, index), "MATERIALIZED") {
			index++
		}
		if !isPunct(tokenAt(tokens, index), '(') || !opensStatement(tokenAt(tokens, index+1)) {
			return index
		}
		index = afterClose[index]
		index = afterClause(tokens, index, "SEARCH", "SET")
		index = afterClause(tokens, index, "CYCLE", "USING")
		from := index
		if recursive {
			from = withIndex
		}
		*scopes = append(*scopes, cteScope{name: foldedName(name), from: from, to: scopeEnd})
		if !isPunct(tokenAt(tokens, index), ',') {
			return index
		}
		index++
	}
}

func opensStatement(token sqlToken) bool {
	if isPunct(token, '(') {
		return true
	}
	return token.kind == tokenWord && statementKeywords[upperASCII(token.text)]
}

// afterClause skips a SEARCH or CYCLE clause of a common table expression. Each
// ends with its last keyword and one column name. A clause holds no parenthesis
// in the forms it reads, and stopping at one keeps text without lastKeyword
// from being scanned again by every WITH.
func afterClause(tokens []sqlToken, from int, firstKeyword, lastKeyword string) int {
	if !isKeyword(tokenAt(tokens, from), firstKeyword) {
		return from
	}
	index := from + 1
	for index < len(tokens) && !isKeyword(tokens[index], lastKeyword) && !isPunct(tokens[index], '(') {
		index++
	}
	if isKeyword(tokenAt(tokens, index), lastKeyword) {
		return index + 2
	}
	return index
}

type scopeChange struct {
	name  string
	delta int
}

// scopeChanges lists the scopes that open (+1) or close (-1) at each token.
// tableNames walks the tokens once and keeps a count per name. Many scopes of
// one name then cost no scan at each table keyword.
func scopeChanges(scopes []cteScope) map[int][]scopeChange {
	changes := map[int][]scopeChange{}
	for _, scope := range scopes {
		changes[scope.from] = append(changes[scope.from], scopeChange{scope.name, 1})
		changes[scope.to] = append(changes[scope.to], scopeChange{scope.name, -1})
	}
	return changes
}

func opOf(tokens []sqlToken, index int) string {
	first := tokenAt(tokens, index)
	if first.kind != tokenWord {
		return ""
	}
	op := upperASCII(first.text)
	if op == "START" && isKeyword(tokenAt(tokens, index+1), "TRANSACTION") {
		return "START TRANSACTION"
	}
	return op
}

func tableNames(tokens []sqlToken, scopes []cteScope, afterExpressions map[int]bool) []string {
	tables := []string{}
	seen := map[string]bool{}
	changes := scopeChanges(scopes)
	visibleScopes := map[string]int{}
	for index := range tokens {
		for _, change := range changes[index] {
			visibleScopes[change.name] += change.delta
		}
		if !isTableKeyword(tokens, index) {
			continue
		}
		parts := qualifiedName(tokens, index+1)
		if len(parts) == 0 {
			continue
		}
		if len(parts) == 1 && !isWriteTarget(tokens, index, afterExpressions) &&
			visibleScopes[foldedName(parts[0])] > 0 {
			continue
		}
		names := make([]string, len(parts))
		for i, part := range parts {
			names[i] = part.text
		}
		table := strings.Join(names, ".")
		if !seen[table] {
			seen[table] = true
			tables = append(tables, table)
		}
	}
	return tables
}

// Postgres looks up the table of DELETE FROM, INSERT INTO, MERGE INTO and
// UPDATE without looking at common table expressions.
func isWriteTarget(tokens []sqlToken, keywordIndex int, afterExpressions map[int]bool) bool {
	keyword := tokens[keywordIndex]
	if isKeyword(keyword, "FROM") {
		return isDeleteCommand(tokens, keywordIndex-1, afterExpressions)
	}
	return !isKeyword(keyword, "JOIN")
}

// isDeleteCommand tells the DELETE command from a column named delete. With a
// common table expression in sight, the command opens the body of an
// expression or follows the list of expressions.
func isDeleteCommand(tokens []sqlToken, index int, afterExpressions map[int]bool) bool {
	return isKeyword(tokenAt(tokens, index), "DELETE") &&
		(isPunct(tokenAt(tokens, index-1), '(') || afterExpressions[index])
}

func isTableKeyword(tokens []sqlToken, index int) bool {
	token := tokens[index]
	if token.kind != tokenWord {
		return false
	}
	keyword := upperASCII(token.text)
	if !tableKeywords[keyword] {
		return false
	}
	if keyword == "FROM" {
		return !isDistinctFromOperator(tokens, index)
	}
	if keyword != "UPDATE" {
		return true
	}
	previous := tokenAt(tokens, index-1)
	return !(previous.kind == tokenWord && updateWithoutTable[upperASCII(previous.text)])
}

// a IS [NOT] DISTINCT FROM b compares two values.
func isDistinctFromOperator(tokens []sqlToken, fromIndex int) bool {
	return isKeyword(tokenAt(tokens, fromIndex-1), "DISTINCT") &&
		(isKeyword(tokenAt(tokens, fromIndex-2), "IS") || isKeyword(tokenAt(tokens, fromIndex-2), "NOT"))
}

// firstNameIndex skips what can sit between a table keyword and the name it
// introduces: LATERAL, ONLY, and the parenthesis of ONLY (name).
func firstNameIndex(tokens []sqlToken, index int) int {
	if isKeyword(tokenAt(tokens, index), "LATERAL") {
		return index + 1
	}
	if !isKeyword(tokenAt(tokens, index), "ONLY") {
		return index
	}
	if isPunct(tokenAt(tokens, index+1), '(') {
		return index + 2
	}
	return index + 1
}

// qualifiedName reads name or schema.name starting at index. A subquery, which
// starts with a parenthesis, has no name.
func qualifiedName(tokens []sqlToken, index int) []sqlToken {
	var parts []sqlToken
	position := firstNameIndex(tokens, index)
	for position < len(tokens) && tokens[position].kind != tokenPunct {
		parts = append(parts, tokens[position])
		if !isPunct(tokenAt(tokens, position+1), '.') {
			break
		}
		position += 2
	}
	return parts
}
