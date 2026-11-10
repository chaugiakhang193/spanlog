package spanlog

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"slices"
)

// errStatementNotRun ends the lines of a statement that database/sql prepared
// after driver.ErrSkip and closed before running it, as when its arguments do
// not match the statement.
var errStatementNotRun = errors.New("spanlog: database/sql closed the statement before running it")

// Connector returns a driver.Connector that writes query lines for the
// statements and transactions that database/sql runs through inner:
//
//	db := sql.OpenDB(log.Connector(connector))
//
// Each statement writes a START line before the driver runs it and an end line
// after. A query ends when its rows are closed, and fails when reading a row
// fails. A transaction writes BEGIN, COMMIT and ROLLBACK lines in the frame of
// the context given to BeginTx. The ROLLBACK line names the tables written in
// the transaction. The connections behave as those of inner, with the same
// optional interfaces of database/sql/driver.
func (l *Logger) Connector(inner driver.Connector) driver.Connector {
	return &loggedConnector{inner: inner, logger: l}
}

type loggedConnector struct {
	inner  driver.Connector
	logger *Logger
}

func (c *loggedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wrapConn(&loggedConn{inner: conn, logger: c.logger}), nil
}

func (c *loggedConnector) Driver() driver.Driver {
	return c.inner.Driver()
}

// DB.Close closes a connector that implements io.Closer.
func (c *loggedConnector) Close() error {
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// database/sql keeps a connection after a rollback caused by a cancelled
// context only when the connection implements both driver.SessionResetter and
// driver.Validator. wrapConn gives the wrapper each of the two methods
// exactly when inner has it, and database/sql chooses as it would for inner.
func wrapConn(conn *loggedConn) driver.Conn {
	resetter, resets := conn.inner.(driver.SessionResetter)
	validator, validates := conn.inner.(driver.Validator)
	switch {
	case resets && validates:
		return &resettingValidConn{loggedConn: conn, resetter: resetter, validator: validator}
	case resets:
		return &resettingConn{loggedConn: conn, resetter: resetter}
	case validates:
		return &validConn{loggedConn: conn, validator: validator}
	default:
		return conn
	}
}

type resettingConn struct {
	*loggedConn
	resetter driver.SessionResetter
}

func (c *resettingConn) ResetSession(ctx context.Context) error {
	return c.resetter.ResetSession(ctx)
}

type validConn struct {
	*loggedConn
	validator driver.Validator
}

func (c *validConn) IsValid() bool {
	return c.validator.IsValid()
}

type resettingValidConn struct {
	*loggedConn
	resetter  driver.SessionResetter
	validator driver.Validator
}

func (c *resettingValidConn) ResetSession(ctx context.Context) error {
	return c.resetter.ResetSession(ctx)
}

func (c *resettingValidConn) IsValid() bool {
	return c.validator.IsValid()
}

// loggedConn wraps one connection of inner. Its fields need no lock of their
// own, because database/sql locks a connection around every call into its
// driver. That lock covers one call, not a whole statement, and goroutines
// that share a Tx or a Conn run their calls on the connection in any order.
type loggedConn struct {
	inner       driver.Conn
	logger      *Logger
	transaction transactionTables
	// skipped holds the statements that inner sent back with driver.ErrSkip.
	// database/sql then prepares the same text with the same context and runs
	// it, and that run keeps the lines already started. Calls of other
	// goroutines can come in between. The slice can hold several statements.
	skipped []*runningStatement
}

// runningStatement is one statement between its START line and its end line.
type runningStatement struct {
	ctx   context.Context
	query string
	span  *QuerySpan
	ended bool
}

func (c *loggedConn) start(ctx context.Context, query string) *runningStatement {
	var options []QueryOption
	if undone, isRollback := c.transaction.statementStarting(query); isRollback {
		options = append(options, WithRolledBackTables(undone))
	}
	return &runningStatement{ctx: ctx, query: query, span: c.logger.StartQuery(ctx, query, options...)}
}

func (c *loggedConn) finish(statement *runningStatement, err error) {
	if statement.ended {
		return
	}
	statement.ended = true
	if err == nil {
		c.transaction.statementDone(statement.query)
	}
	statement.span.Fail(err)
}

// takeSkipped removes and returns the skipped statement that a prepare of
// query with ctx follows, or nil when there is none.
func (c *loggedConn) takeSkipped(ctx context.Context, query string) *runningStatement {
	for index, statement := range c.skipped {
		if statement.query == query && sameContext(statement.ctx, ctx) {
			c.skipped = slices.Delete(c.skipped, index, index+1)
			return statement
		}
	}
	return nil
}

// sameContext compares two contexts without the panic of == on a context
// whose type cannot be compared. Such a context matches any other, and a
// statement still finds its own skipped lines when it is the only one waiting.
func sameContext(a, b context.Context) bool {
	if !reflect.ValueOf(a).Comparable() || !reflect.ValueOf(b).Comparable() {
		return true
	}
	return a == b
}

// endSkipped ends the skipped statements that nothing prepared. database/sql
// always prepares them before it lets the connection go. Only calls made on
// the connection outside database/sql, as through Conn.Raw, leave any.
func (c *loggedConn) endSkipped() {
	for _, statement := range c.skipped {
		c.finish(statement, driver.ErrSkip)
	}
	c.skipped = nil
}

func (c *loggedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, hasContext := c.inner.(driver.ExecerContext)
	plainExecer, hasPlain := c.inner.(driver.Execer)
	if !hasContext && !hasPlain {
		return nil, driver.ErrSkip
	}
	statement := c.start(ctx, query)
	var result driver.Result
	var err error
	if hasContext {
		result, err = execer.ExecContext(ctx, query, args)
	} else {
		result, err = execPlain(ctx, plainExecer, query, args)
	}
	if err == driver.ErrSkip {
		c.skipped = append(c.skipped, statement)
		return nil, err
	}
	c.finish(statement, err)
	return result, err
}

func (c *loggedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, hasContext := c.inner.(driver.QueryerContext)
	plainQueryer, hasPlain := c.inner.(driver.Queryer)
	if !hasContext && !hasPlain {
		return nil, driver.ErrSkip
	}
	statement := c.start(ctx, query)
	var rows driver.Rows
	var err error
	if hasContext {
		rows, err = queryer.QueryContext(ctx, query, args)
	} else {
		rows, err = queryPlain(ctx, plainQueryer, query, args)
	}
	if err == driver.ErrSkip {
		c.skipped = append(c.skipped, statement)
		return nil, err
	}
	if err != nil {
		c.finish(statement, err)
		return nil, err
	}
	return &loggedRows{inner: rows, conn: c, statement: statement}, nil
}

func (c *loggedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	pending := c.takeSkipped(ctx, query)
	stmt, err := prepare(ctx, c.inner, query)
	if err != nil {
		if pending != nil {
			c.finish(pending, err)
		}
		return nil, err
	}
	return wrapStmt(&loggedStmt{inner: stmt, conn: c, query: query, pending: pending}), nil
}

func (c *loggedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *loggedConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	span := c.logger.StartQuery(ctx, "BEGIN")
	tx, err := begin(ctx, c.inner, options)
	if err != nil {
		span.Fail(err)
		return nil, err
	}
	c.transaction.begin()
	span.End()
	return &loggedTx{inner: tx, conn: c, ctx: ctx}, nil
}

func (c *loggedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *loggedConn) Close() error {
	c.endSkipped()
	return c.inner.Close()
}

// database/sql treats a connection without driver.Pinger as alive.
func (c *loggedConn) Ping(ctx context.Context) error {
	if pinger, ok := c.inner.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

// driver.ErrSkip sends database/sql on to the checks it runs when the
// connection has no driver.NamedValueChecker.
func (c *loggedConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.inner.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

// loggedTx carries the context of BeginTx, because driver.Tx has none for
// Commit and Rollback.
type loggedTx struct {
	inner driver.Tx
	conn  *loggedConn
	ctx   context.Context
}

// Postgres ends the transaction even when COMMIT fails.
func (t *loggedTx) Commit() error {
	span := t.conn.logger.StartQuery(t.ctx, "COMMIT")
	err := t.inner.Commit()
	t.conn.transaction.end()
	span.Fail(err)
	return err
}

func (t *loggedTx) Rollback() error {
	undone := t.conn.transaction.rollBack()
	span := t.conn.logger.StartQuery(t.ctx, "ROLLBACK", WithRolledBackTables(undone))
	err := t.inner.Rollback()
	span.Fail(err)
	return err
}

// loggedStmt ends the lines of each run of a prepared statement. pending holds
// the lines a skipped ExecContext or QueryContext started, which the first run
// keeps.
type loggedStmt struct {
	inner   driver.Stmt
	conn    *loggedConn
	query   string
	pending *runningStatement
}

// wrapStmt keeps driver.ColumnConverter only when inner has it, because
// database/sql checks arguments differently without it.
func wrapStmt(stmt *loggedStmt) driver.Stmt {
	if converter, ok := stmt.inner.(driver.ColumnConverter); ok {
		return &convertingStmt{loggedStmt: stmt, converter: converter}
	}
	return stmt
}

type convertingStmt struct {
	*loggedStmt
	converter driver.ColumnConverter
}

func (s *convertingStmt) ColumnConverter(index int) driver.ValueConverter {
	return s.converter.ColumnConverter(index)
}

func (s *loggedStmt) startRun(ctx context.Context) *runningStatement {
	if s.pending != nil {
		statement := s.pending
		s.pending = nil
		return statement
	}
	return s.conn.start(ctx, s.query)
}

func (s *loggedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	statement := s.startRun(ctx)
	result, err := stmtExec(ctx, s.inner, args)
	s.conn.finish(statement, err)
	return result, err
}

func (s *loggedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	statement := s.startRun(ctx)
	rows, err := stmtQuery(ctx, s.inner, args)
	if err != nil {
		s.conn.finish(statement, err)
		return nil, err
	}
	return &loggedRows{inner: rows, conn: s.conn, statement: statement}, nil
}

func (s *loggedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), toNamedValues(args))
}

func (s *loggedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), toNamedValues(args))
}

func (s *loggedStmt) NumInput() int {
	return s.inner.NumInput()
}

func (s *loggedStmt) Close() error {
	if s.pending != nil {
		s.conn.finish(s.pending, errStatementNotRun)
		s.pending = nil
	}
	return s.inner.Close()
}

// database/sql asks the statement first and the connection second.
func (s *loggedStmt) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := s.inner.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return s.conn.CheckNamedValue(value)
}

// loggedRows ends the lines of a query when database/sql closes the rows, which
// it does after the last row, after an error and on Rows.Close.
type loggedRows struct {
	inner     driver.Rows
	conn      *loggedConn
	statement *runningStatement
	// failure is the first error of Next other than io.EOF.
	failure error
}

func (r *loggedRows) Columns() []string {
	return r.inner.Columns()
}

func (r *loggedRows) Next(dest []driver.Value) error {
	err := r.inner.Next(dest)
	if err != nil && err != io.EOF && r.failure == nil {
		r.failure = err
	}
	return err
}

func (r *loggedRows) Close() error {
	err := r.inner.Close()
	failure := r.failure
	if failure == nil {
		failure = err
	}
	r.conn.finish(r.statement, failure)
	return err
}

// The methods below return what database/sql uses when the rows of inner lack
// the matching interface.

func (r *loggedRows) HasNextResultSet() bool {
	if sets, ok := r.inner.(driver.RowsNextResultSet); ok {
		return sets.HasNextResultSet()
	}
	return false
}

func (r *loggedRows) NextResultSet() error {
	if sets, ok := r.inner.(driver.RowsNextResultSet); ok {
		err := sets.NextResultSet()
		if err != nil && err != io.EOF && r.failure == nil {
			r.failure = err
		}
		return err
	}
	return io.EOF
}

func (r *loggedRows) ColumnTypeScanType(index int) reflect.Type {
	if typed, ok := r.inner.(driver.RowsColumnTypeScanType); ok {
		return typed.ColumnTypeScanType(index)
	}
	return reflect.TypeFor[any]()
}

func (r *loggedRows) ColumnTypeDatabaseTypeName(index int) string {
	if typed, ok := r.inner.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return typed.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *loggedRows) ColumnTypeLength(index int) (int64, bool) {
	if typed, ok := r.inner.(driver.RowsColumnTypeLength); ok {
		return typed.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *loggedRows) ColumnTypeNullable(index int) (bool, bool) {
	if typed, ok := r.inner.(driver.RowsColumnTypeNullable); ok {
		return typed.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *loggedRows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if typed, ok := r.inner.(driver.RowsColumnTypePrecisionScale); ok {
		return typed.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

// The helpers below run inner the way database/sql runs a driver without the
// context interfaces (Go 1.26 database/sql/ctxutil.go).

func execPlain(ctx context.Context, execer driver.Execer, query string, args []driver.NamedValue) (driver.Result, error) {
	values, err := namedValuesToValues(args)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return execer.Exec(query, values)
}

func queryPlain(ctx context.Context, queryer driver.Queryer, query string, args []driver.NamedValue) (driver.Rows, error) {
	values, err := namedValuesToValues(args)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return queryer.Query(query, values)
}

func prepare(ctx context.Context, conn driver.Conn, query string) (driver.Stmt, error) {
	if preparer, ok := conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	stmt, err := conn.Prepare(query)
	if err == nil && ctx.Err() != nil {
		stmt.Close()
		return nil, ctx.Err()
	}
	return stmt, err
}

func begin(ctx context.Context, conn driver.Conn, options driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, options)
	}
	if options.Isolation != driver.IsolationLevel(0) {
		return nil, errors.New("sql: driver does not support non-default isolation level")
	}
	if options.ReadOnly {
		return nil, errors.New("sql: driver does not support read-only transactions")
	}
	tx, err := conn.Begin()
	if err == nil && ctx.Err() != nil {
		tx.Rollback()
		return nil, ctx.Err()
	}
	return tx, err
}

func stmtExec(ctx context.Context, stmt driver.Stmt, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := stmt.(driver.StmtExecContext); ok {
		return execer.ExecContext(ctx, args)
	}
	values, err := namedValuesToValues(args)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return stmt.Exec(values)
}

func stmtQuery(ctx context.Context, stmt driver.Stmt, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := stmt.(driver.StmtQueryContext); ok {
		return queryer.QueryContext(ctx, args)
	}
	values, err := namedValuesToValues(args)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return stmt.Query(values)
}

func namedValuesToValues(named []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(named))
	for index, param := range named {
		if param.Name != "" {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		values[index] = param.Value
	}
	return values, nil
}

func toNamedValues(values []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(values))
	for index, value := range values {
		named[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
	}
	return named
}
