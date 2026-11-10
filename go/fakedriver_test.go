package spanlog

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"sync"
)

// fakeBackend records what database/sql asks of the fake driver and answers
// from its settings. Statements are matched by their text.
type fakeBackend struct {
	mu    sync.Mutex
	calls []string
	// failures makes a statement fail when it runs.
	failures map[string]error
	// rowFailures makes Next fail on the second row of a query.
	rowFailures map[string]error
	// closeFailures makes Close of the rows of a query fail.
	closeFailures map[string]error
	// resultSetFailures gives a query a second result set that fails.
	resultSetFailures map[string]error
	// prepareFailures makes preparing a statement fail.
	prepareFailures map[string]error
	// skipWithArgs makes ExecContext and QueryContext answer driver.ErrSkip
	// for a statement with arguments, as a driver does that prepares those.
	skipWithArgs bool
	// numInput is what a prepared statement reports, -1 for unknown.
	numInput    int
	beginErr    error
	commitErr   error
	rollbackErr error
	// typed makes queries answer rows with every column type interface.
	typed bool
	// during runs inside each driver call, after the call is recorded.
	during func(call string)
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		failures:          map[string]error{},
		rowFailures:       map[string]error{},
		closeFailures:     map[string]error{},
		resultSetFailures: map[string]error{},
		prepareFailures:   map[string]error{},
		numInput:          -1,
	}
}

func (b *fakeBackend) record(call string) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	during := b.during
	b.mu.Unlock()
	if during != nil {
		during(call)
	}
}

func (b *fakeBackend) recorded() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.calls...)
}

func (b *fakeBackend) run(kind, query string) error {
	b.record(kind + " " + query)
	return b.failures[query]
}

func (b *fakeBackend) rows(query string) driver.Rows {
	rows := fakeRows{
		values:   [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}},
		failure:  b.rowFailures[query],
		closeErr: b.closeFailures[query],
	}
	if err, ok := b.resultSetFailures[query]; ok {
		return &failingSetRows{fakeRows: rows, setErr: err}
	}
	if b.typed {
		return &typedRows{rows}
	}
	return &rows
}

type fakeConnector struct {
	backend *fakeBackend
	minimal bool
	plain   bool
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	if c.minimal {
		return &minimalConn{backend: c.backend}, nil
	}
	if c.plain {
		return &plainConn{minimalConn{backend: c.backend}}, nil
	}
	return &fullConn{minimalConn{backend: c.backend}}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return fakeDriver{}
}

type closingConnector struct {
	fakeConnector
	closed bool
}

func (c *closingConnector) Close() error {
	c.closed = true
	return nil
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake driver opens connections through its connector")
}

// minimalConn has only the methods of driver.Conn, as the oldest drivers do.
type minimalConn struct {
	backend *fakeBackend
}

func (c *minimalConn) Prepare(query string) (driver.Stmt, error) {
	c.backend.record("prepare " + query)
	if err := c.backend.prepareFailures[query]; err != nil {
		return nil, err
	}
	return &fakeStmt{backend: c.backend, query: query}, nil
}

func (c *minimalConn) Close() error {
	return nil
}

func (c *minimalConn) Begin() (driver.Tx, error) {
	c.backend.record("begin")
	if c.backend.beginErr != nil {
		return nil, c.backend.beginErr
	}
	return &fakeTx{backend: c.backend}, nil
}

// fullConn adds the context interfaces that current drivers have.
type fullConn struct {
	minimalConn
}

func (c *fullConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.backend.skipWithArgs && len(args) > 0 {
		return nil, driver.ErrSkip
	}
	if err := c.backend.run("exec", query); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func (c *fullConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.backend.skipWithArgs && len(args) > 0 {
		return nil, driver.ErrSkip
	}
	if err := c.backend.run("query", query); err != nil {
		return nil, err
	}
	return c.backend.rows(query), nil
}

func (c *fullConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.backend.record("begin")
	if c.backend.beginErr != nil {
		return nil, c.backend.beginErr
	}
	return &fakeTx{backend: c.backend}, nil
}

// plainConn runs statements through driver.Execer and driver.Queryer, which
// have no context.
type plainConn struct {
	minimalConn
}

func (c *plainConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if err := c.backend.run("plain exec", query); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func (c *plainConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if err := c.backend.run("plain query", query); err != nil {
		return nil, err
	}
	return c.backend.rows(query), nil
}

type fakeStmt struct {
	backend *fakeBackend
	query   string
}

func (s *fakeStmt) Close() error {
	s.backend.record("close " + s.query)
	return nil
}

func (s *fakeStmt) NumInput() int {
	return s.backend.numInput
}

func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	if err := s.backend.run("stmt exec", s.query); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	if err := s.backend.run("stmt query", s.query); err != nil {
		return nil, err
	}
	return s.backend.rows(s.query), nil
}

type fakeTx struct {
	backend *fakeBackend
}

func (t *fakeTx) Commit() error {
	t.backend.record("commit")
	return t.backend.commitErr
}

func (t *fakeTx) Rollback() error {
	t.backend.record("rollback")
	return t.backend.rollbackErr
}

type fakeRows struct {
	values   [][]driver.Value
	next     int
	failure  error
	closeErr error
}

func (r *fakeRows) Columns() []string {
	return []string{"n"}
}

func (r *fakeRows) Close() error {
	return r.closeErr
}

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.failure != nil && r.next == 1 {
		return r.failure
	}
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

// typedRows has every column type interface of database/sql/driver.
type typedRows struct {
	fakeRows
}

func (r *typedRows) ColumnTypeScanType(int) reflect.Type   { return reflect.TypeFor[int64]() }
func (r *typedRows) ColumnTypeDatabaseTypeName(int) string { return "INT8" }
func (r *typedRows) ColumnTypeLength(int) (int64, bool)    { return 8, true }
func (r *typedRows) ColumnTypeNullable(int) (bool, bool)   { return true, true }
func (r *typedRows) ColumnTypePrecisionScale(int) (int64, int64, bool) {
	return 19, 0, true
}
func (r *typedRows) HasNextResultSet() bool { return false }
func (r *typedRows) NextResultSet() error   { return io.EOF }

// failingSetRows announces a second result set and fails to move to it.
type failingSetRows struct {
	fakeRows
	setErr error
}

func (r *failingSetRows) HasNextResultSet() bool { return true }
func (r *failingSetRows) NextResultSet() error   { return r.setErr }

// connConnector opens connections made by newConn, for connection types that
// fakeConnector does not choose between.
type connConnector struct {
	newConn func() driver.Conn
}

func (c connConnector) Connect(context.Context) (driver.Conn, error) {
	return c.newConn(), nil
}

func (c connConnector) Driver() driver.Driver {
	return fakeDriver{}
}

// cartID is accepted as an argument only by a driver.NamedValueChecker, since
// database/sql rejects a struct that is not a driver.Valuer.
type cartID struct {
	n int64
}

func checkCartID(value *driver.NamedValue) error {
	if id, ok := value.Value.(cartID); ok {
		value.Value = id.n
		return nil
	}
	return driver.ErrSkip
}

// checkingConn checks arguments on the connection.
type checkingConn struct {
	fullConn
}

func (c *checkingConn) CheckNamedValue(value *driver.NamedValue) error {
	return checkCartID(value)
}

// checkingStmtConn prepares statements that check their arguments.
type checkingStmtConn struct {
	fullConn
}

func (c *checkingStmtConn) Prepare(query string) (driver.Stmt, error) {
	c.backend.record("prepare " + query)
	return &checkingStmt{fakeStmt{backend: c.backend, query: query}}, nil
}

type checkingStmt struct {
	fakeStmt
}

func (s *checkingStmt) CheckNamedValue(value *driver.NamedValue) error {
	return checkCartID(value)
}

// pingingConn answers Ping with err.
type pingingConn struct {
	fullConn
	err error
}

func (c *pingingConn) Ping(context.Context) error {
	return c.err
}

// convertingStmtConn prepares statements with a driver.ColumnConverter.
type convertingStmtConn struct {
	fullConn
}

func (c *convertingStmtConn) Prepare(query string) (driver.Stmt, error) {
	c.backend.record("prepare " + query)
	return &convertingFakeStmt{fakeStmt{backend: c.backend, query: query}}, nil
}

type convertingFakeStmt struct {
	fakeStmt
}

func (s *convertingFakeStmt) ColumnConverter(int) driver.ValueConverter {
	return cartIDConverter{}
}

type cartIDConverter struct{}

func (cartIDConverter) ConvertValue(value any) (driver.Value, error) {
	if id, ok := value.(cartID); ok {
		return id.n, nil
	}
	return driver.DefaultParameterConverter.ConvertValue(value)
}
