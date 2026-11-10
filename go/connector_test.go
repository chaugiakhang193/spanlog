package spanlog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func openLoggedDB(t *testing.T, backend *fakeBackend, minimal bool) (*sql.DB, *recordingSink, *Logger) {
	t.Helper()
	log, sink := newTestLogger(Options{})
	db := sql.OpenDB(log.Connector(&fakeConnector{backend: backend, minimal: minimal}))
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, sink, log
}

func TestConnectorWritesTheLinesOfAStatement(t *testing.T) {
	backend := newFakeBackend()
	db, sink, log := openLoggedDB(t, backend, false)

	err := inFrame(log, func(ctx context.Context) error {
		_, err := db.ExecContext(ctx, `INSERT INTO "orders"("id") VALUES ($1)`, 7)
		return err
	})

	if err != nil {
		t.Fatal(err)
	}
	assertShapes(t, sink,
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query INSERT orders | START",
		"debug OrderService.Place - query INSERT orders | DONE Nms",
		"info OrderService.Place - run | DONE Nms",
	)
}

func TestConnectorWritesAFailedStatement(t *testing.T) {
	backend := newFakeBackend()
	backend.failures["INSERT INTO orders (id) VALUES ($1)"] = errors.New("duplicate key")
	db, sink, _ := openLoggedDB(t, backend, false)

	_, err := db.ExecContext(context.Background(), "INSERT INTO orders (id) VALUES ($1)", 7)

	if err == nil || err.Error() != "duplicate key" {
		t.Fatalf("err = %v", err)
	}
	assertShapes(t, sink,
		"debug query INSERT orders | START",
		"warn query INSERT orders | FAILED Nms duplicate key",
	)
}

func TestConnectorEndsAQueryWhenItsRowsClose(t *testing.T) {
	backend := newFakeBackend()
	db, sink, _ := openLoggedDB(t, backend, false)

	rows, err := db.QueryContext(context.Background(), "SELECT n FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	assertShapes(t, sink, "debug query SELECT numbers | START")
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if count != 3 {
		t.Errorf("read %d rows", count)
	}
	assertShapes(t, sink, "debug query SELECT numbers | START", "debug query SELECT numbers | DONE Nms")
	rows.Close()
	assertShapes(t, sink, "debug query SELECT numbers | START", "debug query SELECT numbers | DONE Nms")
}

// Postgres reports an error such as a division by zero with the row that
// causes it, after earlier rows have arrived.
func TestConnectorFailsAQueryWhenReadingARowFails(t *testing.T) {
	backend := newFakeBackend()
	backend.rowFailures["SELECT 1 / (n - 2) FROM numbers"] = errors.New("division by zero")
	db, sink, _ := openLoggedDB(t, backend, false)

	rows, err := db.QueryContext(context.Background(), "SELECT 1 / (n - 2) FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}

	if rows.Err() == nil {
		t.Fatal("rows.Err() = nil")
	}
	assertShapes(t, sink,
		"debug query SELECT numbers | START",
		"warn query SELECT numbers | FAILED Nms division by zero",
	)
}

func TestConnectorWritesTheLinesOfACommittedTransaction(t *testing.T) {
	backend := newFakeBackend()
	db, sink, log := openLoggedDB(t, backend, false)

	err := inFrame(log, func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "INSERT INTO orders (id) VALUES ($1)", 7); err != nil {
			return err
		}
		return tx.Commit()
	})

	if err != nil {
		t.Fatal(err)
	}
	assertShapes(t, sink,
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query BEGIN | START",
		"debug OrderService.Place - query BEGIN | DONE Nms",
		"debug OrderService.Place - query INSERT orders | START",
		"debug OrderService.Place - query INSERT orders | DONE Nms",
		"debug OrderService.Place - query COMMIT | START",
		"debug OrderService.Place - query COMMIT | DONE Nms",
		"info OrderService.Place - run | DONE Nms",
	)
	if got := backend.recorded(); !reflect.DeepEqual(got, []string{"begin", "exec INSERT INTO orders (id) VALUES ($1)", "commit"}) {
		t.Errorf("driver calls = %q", got)
	}
}

func TestConnectorNamesTheTablesARolledBackTransactionWrote(t *testing.T) {
	backend := newFakeBackend()
	backend.failures["UPDATE warehouse.inventory SET available = available - 1"] = errors.New("check constraint")
	db, sink, log := openLoggedDB(t, backend, false)

	_ = inFrame(log, func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "INSERT INTO orders (id) VALUES ($1)", 7); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE warehouse.inventory SET available = available - 1")
		return err
	})

	got := sink.shapes()
	want := []string{
		"debug OrderService.Place - query ROLLBACK orders | START",
		"warn OrderService.Place - query ROLLBACK orders | DONE Nms",
	}
	if len(got) < 3 || !reflect.DeepEqual(got[len(got)-3:len(got)-1], want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
	if fields := sink.seen[len(sink.seen)-2].Fields; !reflect.DeepEqual(fields.Tables, []string{"orders"}) {
		t.Errorf("tables = %q", fields.Tables)
	}
}

// A ROLLBACK sent as a statement after Commit has no transaction to undo.
func TestConnectorForgetsTheTablesOfACommittedTransaction(t *testing.T) {
	backend := newFakeBackend()
	db, sink, _ := openLoggedDB(t, backend, false)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO orders (id) VALUES ($1)", 7); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	got := sink.shapes()
	if last := got[len(got)-1]; last != "warn query ROLLBACK | DONE Nms" {
		t.Errorf("last line = %q", last)
	}
}

func TestConnectorWritesAFailedRollback(t *testing.T) {
	backend := newFakeBackend()
	backend.rollbackErr = errors.New("connection reset")
	db, sink, _ := openLoggedDB(t, backend, false)

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err == nil {
		t.Fatal("Rollback returned nil")
	}

	assertShapes(t, sink,
		"debug query BEGIN | START",
		"debug query BEGIN | DONE Nms",
		"debug query ROLLBACK | START",
		"warn query ROLLBACK | FAILED Nms connection reset",
	)
}

// database/sql rolls back a transaction whose context ends, on a goroutine of
// its own. The line belongs to the frame that began the transaction.
func TestConnectorWritesTheRollbackOfACancelledTransaction(t *testing.T) {
	backend := newFakeBackend()
	db, sink, log := openLoggedDB(t, backend, false)

	want := "warn OrderService.Place - query ROLLBACK carts | DONE Nms"
	_ = inFrame(log, func(ctx context.Context) error {
		txContext, cancel := context.WithCancel(ctx)
		defer cancel()
		tx, err := db.BeginTx(txContext, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(txContext, "DELETE FROM carts WHERE id = $1", 7); err != nil {
			return err
		}
		cancel()
		deadline := time.Now().Add(2 * time.Second)
		for !slices.Contains(sink.shapes(), want) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return nil
	})

	got := sink.shapes()
	if !slices.Contains(got, want) {
		t.Errorf("lines:\n%s\nwant a line %q", strings.Join(got, "\n"), want)
	}
}

// Drivers such as one that sends arguments only through prepared statements
// answer driver.ErrSkip, and database/sql prepares the statement and runs it.
// The statement still writes one START line and one end line.
func TestConnectorWritesOnePairForAStatementTheDriverSkips(t *testing.T) {
	for _, run := range []struct {
		name string
		do   func(ctx context.Context, db *sql.DB) error
	}{
		{"exec", func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", 7)
			return err
		}},
		{"query", func(ctx context.Context, db *sql.DB) error {
			rows, err := db.QueryContext(ctx, "SELECT n FROM carts WHERE id = $1", 7)
			if err != nil {
				return err
			}
			return rows.Close()
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			backend := newFakeBackend()
			backend.skipWithArgs = true
			db, sink, _ := openLoggedDB(t, backend, false)

			if err := run.do(context.Background(), db); err != nil {
				t.Fatal(err)
			}

			got := sink.shapes()
			if len(got) != 2 || !strings.HasSuffix(got[0], "| START") || !strings.Contains(got[1], "| DONE") {
				t.Errorf("lines:\n%s", strings.Join(got, "\n"))
			}
			calls := backend.recorded()
			if len(calls) == 0 || !strings.HasPrefix(calls[0], "prepare ") {
				t.Errorf("driver calls = %q, want a prepared statement", calls)
			}
		})
	}
}

func TestConnectorEndsASkippedStatementThatDatabaseSQLNeverRuns(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	backend.numInput = 2
	db, sink, _ := openLoggedDB(t, backend, false)

	_, err := db.ExecContext(context.Background(), "DELETE FROM carts WHERE id = $1", 7)

	if err == nil {
		t.Fatal("ExecContext returned nil with one argument for two")
	}
	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"warn query DELETE carts | FAILED Nms spanlog: database/sql closed the statement before running it",
	)
}

func TestConnectorWorksWithAConnectionWithoutContextMethods(t *testing.T) {
	backend := newFakeBackend()
	db, sink, _ := openLoggedDB(t, backend, true)

	if _, err := db.ExecContext(context.Background(), "INSERT INTO orders (id) VALUES ($1)", 7); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	assertShapes(t, sink,
		"debug query INSERT orders | START",
		"debug query INSERT orders | DONE Nms",
		"debug query BEGIN | START",
		"debug query BEGIN | DONE Nms",
		"debug query COMMIT | START",
		"debug query COMMIT | DONE Nms",
	)
}

func TestConnectorRunsAConnectionWithPlainExecAndQuery(t *testing.T) {
	backend := newFakeBackend()
	log, sink := newTestLogger(Options{})
	db := sql.OpenDB(log.Connector(&fakeConnector{backend: backend, plain: true}))
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "DELETE FROM carts"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT n FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()

	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"debug query DELETE carts | DONE Nms",
		"debug query SELECT numbers | START",
		"debug query SELECT numbers | DONE Nms",
	)
	if got := backend.recorded(); !reflect.DeepEqual(got, []string{"plain exec DELETE FROM carts", "plain query SELECT n FROM numbers"}) {
		t.Errorf("driver calls = %q", got)
	}
}

// The same options fail with the same error with and without the wrapper.
func TestConnectorKeepsTheErrorOfUnsupportedTransactionOptions(t *testing.T) {
	for _, options := range []*sql.TxOptions{{Isolation: sql.LevelSerializable}, {ReadOnly: true}} {
		backend := newFakeBackend()
		plain := sql.OpenDB(&fakeConnector{backend: backend, minimal: true})
		logged, _, _ := openLoggedDB(t, backend, true)

		_, plainErr := plain.BeginTx(context.Background(), options)
		_, loggedErr := logged.BeginTx(context.Background(), options)
		plain.Close()

		if plainErr == nil || loggedErr == nil || plainErr.Error() != loggedErr.Error() {
			t.Errorf("options %+v: plain %v, logged %v", options, plainErr, loggedErr)
		}
	}
}

func TestConnectorKeepsTheColumnTypesOfTheDriver(t *testing.T) {
	for _, typed := range []bool{false, true} {
		backend := newFakeBackend()
		backend.typed = typed
		plain := sql.OpenDB(&fakeConnector{backend: backend})
		logged, _, _ := openLoggedDB(t, backend, false)

		plainTypes := columnTypesOf(t, plain)
		loggedTypes := columnTypesOf(t, logged)
		plain.Close()

		if !reflect.DeepEqual(plainTypes, loggedTypes) {
			t.Errorf("typed %v: plain %v, logged %v", typed, plainTypes, loggedTypes)
		}
	}
}

func columnTypesOf(t *testing.T, db *sql.DB) []any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT n FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	column := types[0]
	length, hasLength := column.Length()
	nullable, hasNullable := column.Nullable()
	precision, scale, hasPrecision := column.DecimalSize()
	return []any{column.ScanType(), column.DatabaseTypeName(), length, hasLength, nullable, hasNullable, precision, scale, hasPrecision}
}

type resettingFakeConn struct{ minimalConn }

func (resettingFakeConn) ResetSession(context.Context) error { return nil }

type validFakeConn struct{ minimalConn }

func (validFakeConn) IsValid() bool { return true }

type resettingValidFakeConn struct{ minimalConn }

func (resettingValidFakeConn) ResetSession(context.Context) error { return nil }
func (resettingValidFakeConn) IsValid() bool                      { return true }

// database/sql keeps a connection after a cancelled transaction only when it
// has both methods. The wrapper has each of them exactly when inner has it.
func TestConnectorHasTheSessionMethodsOfTheConnection(t *testing.T) {
	for _, inner := range []driver.Conn{
		&minimalConn{}, &resettingFakeConn{}, &validFakeConn{}, &resettingValidFakeConn{},
	} {
		wrapped := wrapConn(&loggedConn{inner: inner})
		_, innerResets := inner.(driver.SessionResetter)
		_, innerValidates := inner.(driver.Validator)
		_, resets := wrapped.(driver.SessionResetter)
		_, validates := wrapped.(driver.Validator)
		if resets != innerResets || validates != innerValidates {
			t.Errorf("%T: wrapper resets %v validates %v", inner, resets, validates)
		}
	}
}

func TestConnectorClosesAConnectorThatCanClose(t *testing.T) {
	log, _ := newTestLogger(Options{})
	inner := &closingConnector{fakeConnector: fakeConnector{backend: newFakeBackend()}}
	db := sql.OpenDB(log.Connector(inner))

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if !inner.closed {
		t.Error("inner connector stayed open")
	}
}

// database/sql runs a statement that the driver skips in two calls, and other
// goroutines of a Tx or a Conn can run statements on the connection between
// them.
func TestConnectorKeepsASkippedStatementWhileAnotherRuns(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	log, sink := newTestLogger(Options{})
	conn := &loggedConn{inner: &fullConn{minimalConn{backend: backend}}, logger: log}
	ctx := context.Background()
	query := "DELETE FROM carts WHERE id = $1"
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}

	if _, err := conn.ExecContext(ctx, query, args); err != driver.ErrSkip {
		t.Fatalf("err = %v, want driver.ErrSkip", err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM orders", nil); err != nil {
		t.Fatal(err)
	}
	runPrepared(t, conn, ctx, query, args)

	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"debug query DELETE orders | START",
		"debug query DELETE orders | DONE Nms",
		"debug query DELETE carts | DONE Nms",
	)
}

// Two goroutines can wait between the two calls with the same text. Each run
// keeps the START line written in the frame of its own context.
func TestConnectorGivesEachSkippedStatementTheLinesOfItsContext(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	log, sink := newTestLogger(Options{})
	conn := &loggedConn{inner: &fullConn{minimalConn{backend: backend}}, logger: log}
	placeContext, placeDone := log.Start(WithRequest(context.Background(), "r1"), "OrderService", "Place")
	clearContext, clearDone := log.Start(WithRequest(context.Background(), "r2"), "CartService", "Clear")
	query := "DELETE FROM carts WHERE id = $1"
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}

	for _, ctx := range []context.Context{placeContext, clearContext} {
		if _, err := conn.ExecContext(ctx, query, args); err != driver.ErrSkip {
			t.Fatalf("err = %v, want driver.ErrSkip", err)
		}
	}
	runPrepared(t, conn, clearContext, query, args)
	runPrepared(t, conn, placeContext, query, args)
	clearDone(nil)
	placeDone(nil)

	assertShapes(t, sink,
		"info OrderService.Place - run | START",
		"info CartService.Clear - run | START",
		"debug OrderService.Place - query DELETE carts | START",
		"debug CartService.Clear - query DELETE carts | START",
		"debug CartService.Clear - query DELETE carts | DONE Nms",
		"debug OrderService.Place - query DELETE carts | DONE Nms",
		"info CartService.Clear - run | DONE Nms",
		"info OrderService.Place - run | DONE Nms",
	)
}

// runPrepared makes the calls database/sql makes after driver.ErrSkip.
func runPrepared(t *testing.T, conn *loggedConn, ctx context.Context, query string, args []driver.NamedValue) {
	t.Helper()
	stmt, err := conn.PrepareContext(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if _, err := stmt.(driver.StmtExecContext).ExecContext(ctx, args); err != nil {
		t.Fatal(err)
	}
}

// Goroutines that share a Tx run their statements on one connection, and
// database/sql lets their calls interleave.
func TestConnectorWritesOnePairPerStatementOfGoroutinesSharingATx(t *testing.T) {
	const workers, statements = 4, 25
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		backend := newFakeBackend()
		backend.skipWithArgs = true
		log, sink := newTestLogger(Options{})
		db := sql.OpenDB(log.Connector(&fakeConnector{backend: backend}))
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var group sync.WaitGroup
		for worker := range workers {
			// Two workers share each text.
			query := fmt.Sprintf("DELETE FROM carts%d WHERE id = $1", worker%2)
			group.Go(func() {
				for id := range statements {
					if _, err := tx.ExecContext(ctx, query, id); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		group.Wait()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		db.Close()

		starts, dones := 0, 0
		for _, line := range sink.shapes() {
			switch {
			case strings.HasSuffix(line, "| START"):
				starts++
			case strings.HasSuffix(line, "| DONE Nms"):
				dones++
			default:
				t.Fatalf("round %d: line %q", round, line)
			}
		}
		if want := workers*statements + 2; starts != want || dones != want {
			t.Fatalf("round %d: %d START and %d DONE lines, want %d of each", round, starts, dones, want)
		}
	}
}

// Conn.Raw hands the wrapped connection to the caller, and a statement it
// gets driver.ErrSkip for is never prepared.
func TestConnectorEndsASkippedStatementWhenTheConnectionCloses(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	db, sink, _ := openLoggedDB(t, backend, false)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = conn.Raw(func(driverConn any) error {
		args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}
		for _, query := range []string{"DELETE FROM carts WHERE id = $1", "DELETE FROM orders WHERE id = $1"} {
			_, err := driverConn.(driver.ExecerContext).ExecContext(ctx, query, args)
			if err != driver.ErrSkip {
				return fmt.Errorf("err = %v, want driver.ErrSkip", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	assertShapes(t, sink, "debug query DELETE carts | START", "debug query DELETE orders | START")
	db.Close()

	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"debug query DELETE orders | START",
		"warn query DELETE carts | FAILED Nms driver: skip fast-path; continue as if unimplemented",
		"warn query DELETE orders | FAILED Nms driver: skip fast-path; continue as if unimplemented",
	)
}

func TestConnectorWritesAFailedQuery(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip %v", skip), func(t *testing.T) {
			backend := newFakeBackend()
			backend.skipWithArgs = skip
			backend.failures["SELECT n FROM carts WHERE id = $1"] = errors.New("permission denied")
			db, sink, _ := openLoggedDB(t, backend, false)

			_, err := db.QueryContext(context.Background(), "SELECT n FROM carts WHERE id = $1", 7)

			if err == nil || err.Error() != "permission denied" {
				t.Fatalf("err = %v", err)
			}
			assertShapes(t, sink,
				"debug query SELECT carts | START",
				"warn query SELECT carts | FAILED Nms permission denied",
			)
		})
	}
}

func TestConnectorFailsAQueryWhoseRowsFailToClose(t *testing.T) {
	backend := newFakeBackend()
	backend.closeFailures["SELECT n FROM numbers"] = errors.New("unexpected EOF")
	db, sink, _ := openLoggedDB(t, backend, false)

	rows, err := db.QueryContext(context.Background(), "SELECT n FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()

	assertShapes(t, sink,
		"debug query SELECT numbers | START",
		"warn query SELECT numbers | FAILED Nms unexpected EOF",
	)
}

// The error of a row says more about the query than the error of closing
// its rows after it.
func TestConnectorKeepsTheRowErrorOverTheCloseError(t *testing.T) {
	backend := newFakeBackend()
	backend.rowFailures["SELECT 1 / (n - 2) FROM numbers"] = errors.New("division by zero")
	backend.closeFailures["SELECT 1 / (n - 2) FROM numbers"] = errors.New("unexpected EOF")
	db, sink, _ := openLoggedDB(t, backend, false)

	rows, err := db.QueryContext(context.Background(), "SELECT 1 / (n - 2) FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()

	assertShapes(t, sink,
		"debug query SELECT numbers | START",
		"warn query SELECT numbers | FAILED Nms division by zero",
	)
}

func TestConnectorFailsAQueryWhoseNextResultSetFails(t *testing.T) {
	backend := newFakeBackend()
	backend.resultSetFailures["SELECT n FROM numbers"] = errors.New("canceling statement")
	db, sink, _ := openLoggedDB(t, backend, false)

	rows, err := db.QueryContext(context.Background(), "SELECT n FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if rows.NextResultSet() {
		t.Fatal("NextResultSet() = true")
	}
	rows.Close()

	assertShapes(t, sink,
		"debug query SELECT numbers | START",
		"warn query SELECT numbers | FAILED Nms canceling statement",
	)
}

// A driver.NamedValueChecker can accept arguments that database/sql rejects
// by itself. The wrapper must ask the one that inner has.
func TestConnectorChecksArgumentsWithTheCheckerOfTheDriver(t *testing.T) {
	ctx := context.Background()
	execCart := func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", cartID{7})
		return err
	}
	prepareCart := func(db *sql.DB) error {
		stmt, err := db.PrepareContext(ctx, "DELETE FROM carts WHERE id = $1")
		if err != nil {
			return err
		}
		defer stmt.Close()
		_, err = stmt.ExecContext(ctx, cartID{7})
		return err
	}
	for _, run := range []struct {
		name string
		conn func(backend *fakeBackend) driver.Conn
		skip bool
		do   func(db *sql.DB) error
	}{
		{"connection", func(backend *fakeBackend) driver.Conn {
			return &checkingConn{fullConn{minimalConn{backend: backend}}}
		}, false, execCart},
		{"connection after skip", func(backend *fakeBackend) driver.Conn {
			return &checkingConn{fullConn{minimalConn{backend: backend}}}
		}, true, execCart},
		{"statement", func(backend *fakeBackend) driver.Conn {
			return &checkingStmtConn{fullConn{minimalConn{backend: backend}}}
		}, false, prepareCart},
		{"column converter", func(backend *fakeBackend) driver.Conn {
			return &convertingStmtConn{fullConn{minimalConn{backend: backend}}}
		}, false, prepareCart},
	} {

		t.Run(run.name, func(t *testing.T) {
			backend := newFakeBackend()
			backend.skipWithArgs = run.skip
			// A known count makes database/sql call the column converter.
			backend.numInput = 1
			log, sink := newTestLogger(Options{})
			db := sql.OpenDB(log.Connector(connConnector{newConn: func() driver.Conn { return run.conn(backend) }}))
			defer db.Close()

			if err := run.do(db); err != nil {
				t.Fatal(err)
			}

			assertShapes(t, sink, "debug query DELETE carts | START", "debug query DELETE carts | DONE Nms")
		})
	}
}

func TestConnectorPingsThroughTheConnection(t *testing.T) {
	for _, want := range []error{nil, errors.New("server closed the connection")} {
		backend := newFakeBackend()
		log, _ := newTestLogger(Options{})
		db := sql.OpenDB(log.Connector(connConnector{newConn: func() driver.Conn {
			return &pingingConn{fullConn: fullConn{minimalConn{backend: backend}}, err: want}
		}}))

		err := db.PingContext(context.Background())
		db.Close()

		if !errors.Is(err, want) {
			t.Errorf("Ping = %v, want %v", err, want)
		}
	}
}

func TestConnectorWritesAFailedBegin(t *testing.T) {
	backend := newFakeBackend()
	backend.beginErr = errors.New("too many connections")
	db, sink, _ := openLoggedDB(t, backend, false)

	if _, err := db.BeginTx(context.Background(), nil); err == nil {
		t.Fatal("BeginTx returned nil")
	}

	assertShapes(t, sink,
		"debug query BEGIN | START",
		"warn query BEGIN | FAILED Nms too many connections",
	)
}

// Postgres ends the transaction when COMMIT fails. A later ROLLBACK
// statement has no tables to name.
func TestConnectorWritesAFailedCommit(t *testing.T) {
	backend := newFakeBackend()
	backend.commitErr = errors.New("could not serialize access")
	db, sink, _ := openLoggedDB(t, backend, false)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO orders (id) VALUES ($1)", 7); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("Commit returned nil")
	}
	if _, err := db.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	got := sink.shapes()
	want := []string{
		"debug query COMMIT | START",
		"warn query COMMIT | FAILED Nms could not serialize access",
		"debug query ROLLBACK | START",
		"warn query ROLLBACK | DONE Nms",
	}
	if len(got) < len(want) || !reflect.DeepEqual(got[len(got)-len(want):], want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
}

func TestConnectorFailsASkippedStatementThatFailsToPrepare(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	backend.prepareFailures["DELETE FROM carts WHERE id = $1"] = errors.New("syntax error")
	db, sink, _ := openLoggedDB(t, backend, false)

	_, err := db.ExecContext(context.Background(), "DELETE FROM carts WHERE id = $1", 7)

	if err == nil || err.Error() != "syntax error" {
		t.Fatalf("err = %v", err)
	}
	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"warn query DELETE carts | FAILED Nms syntax error",
	)
}

// A connection without the context interfaces gets arguments without names,
// and database/sql reports named ones as unsupported.
func TestConnectorKeepsTheErrorOfNamedArgumentsForAPlainConnection(t *testing.T) {
	backend := newFakeBackend()
	ctx := context.Background()
	plain := sql.OpenDB(&fakeConnector{backend: backend, plain: true})
	defer plain.Close()
	log, _ := newTestLogger(Options{})
	logged := sql.OpenDB(log.Connector(&fakeConnector{backend: backend, plain: true}))
	defer logged.Close()

	_, plainErr := plain.ExecContext(ctx, "DELETE FROM carts WHERE id = @id", sql.Named("id", 7))
	_, loggedErr := logged.ExecContext(ctx, "DELETE FROM carts WHERE id = @id", sql.Named("id", 7))

	if plainErr == nil || loggedErr == nil || plainErr.Error() != loggedErr.Error() {
		t.Errorf("plain %v, logged %v", plainErr, loggedErr)
	}
}

func TestConnectorReturnsTheDriverOfInner(t *testing.T) {
	log, _ := newTestLogger(Options{})
	db := sql.OpenDB(log.Connector(&fakeConnector{backend: newFakeBackend()}))
	defer db.Close()

	if _, ok := db.Driver().(fakeDriver); !ok {
		t.Errorf("Driver() = %T", db.Driver())
	}
}

// A run takes the skipped lines of its own text, whichever of exec and query
// skipped them.
func TestConnectorMatchesASkippedStatementByItsText(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	log, sink := newTestLogger(Options{})
	conn := &loggedConn{inner: &fullConn{minimalConn{backend: backend}}, logger: log}
	ctx := context.Background()
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}

	if _, err := conn.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", args); err != driver.ErrSkip {
		t.Fatalf("exec err = %v, want driver.ErrSkip", err)
	}
	if _, err := conn.QueryContext(ctx, "SELECT n FROM orders WHERE id = $1", args); err != driver.ErrSkip {
		t.Fatalf("query err = %v, want driver.ErrSkip", err)
	}
	stmt, err := conn.PrepareContext(ctx, "SELECT n FROM orders WHERE id = $1")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	stmt.Close()
	runPrepared(t, conn, ctx, "DELETE FROM carts WHERE id = $1", args)

	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"debug query SELECT orders | START",
		"debug query SELECT orders | DONE Nms",
		"debug query DELETE carts | DONE Nms",
	)
}

// For a driver without the context interfaces, database/sql gives up a
// prepared statement or a transaction whose context ends during the call.
// The wrapper does the same, and a statement that never runs writes no line.
func TestConnectorGivesUpWhenTheContextEndsDuringAPlainCall(t *testing.T) {
	for _, run := range []struct {
		call string
		do   func(ctx context.Context, db *sql.DB) error
		want []string
	}{
		{"prepare", func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", 7)
			return err
		}, nil},
		{"begin", func(ctx context.Context, db *sql.DB) error {
			_, err := db.BeginTx(ctx, nil)
			return err
		}, []string{"debug query BEGIN | START", "warn query BEGIN | FAILED Nms context canceled"}},
	} {
		t.Run(run.call, func(t *testing.T) {
			backend := newFakeBackend()
			db, sink, _ := openLoggedDB(t, backend, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			backend.during = func(call string) {
				if strings.HasPrefix(call, run.call) {
					cancel()
				}
			}

			err := run.do(ctx, db)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			assertShapes(t, sink, run.want...)
		})
	}
}

// database/sql checks the context before a call through driver.Execer, which
// has none. Conn.Raw reaches that call with a context that has ended.
func TestConnectorChecksTheContextBeforeAPlainExec(t *testing.T) {
	backend := newFakeBackend()
	log, _ := newTestLogger(Options{})
	db := sql.OpenDB(log.Connector(&fakeConnector{backend: backend, plain: true}))
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	err = conn.Raw(func(driverConn any) error {
		_, err := driverConn.(driver.ExecerContext).ExecContext(ended, "DELETE FROM carts", nil)
		return err
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls := backend.recorded(); len(calls) != 0 {
		t.Errorf("driver calls = %q", calls)
	}
}

// A statement that hangs in the driver must already show its START line, so
// each wrapped call writes START before it calls inner.
func TestConnectorWritesStartBeforeEachDriverCall(t *testing.T) {
	backend := newFakeBackend()
	db, sink, _ := openLoggedDB(t, backend, false)
	lastLineAt := map[string]string{}
	backend.during = func(call string) {
		if shapes := sink.shapes(); len(shapes) > 0 {
			lastLineAt[call] = shapes[len(shapes)-1]
		}
	}
	ctx := context.Background()

	committed, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := committed.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", 7); err != nil {
		t.Fatal(err)
	}
	stmt, err := committed.PrepareContext(ctx, "UPDATE carts SET total = 0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		t.Fatal(err)
	}
	stmt.Close()
	rows, err := committed.QueryContext(ctx, "SELECT id FROM numbers")
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if err := committed.Commit(); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rolledBack.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", 7); err != nil {
		t.Fatal(err)
	}
	if err := rolledBack.Rollback(); err != nil {
		t.Fatal(err)
	}

	for call, want := range map[string]string{
		"begin":                                "debug query BEGIN | START",
		"exec DELETE FROM carts WHERE id = $1": "debug query DELETE carts | START",
		"stmt exec UPDATE carts SET total = 0": "debug query UPDATE carts | START",
		"query SELECT id FROM numbers":         "debug query SELECT numbers | START",
		"commit":                               "debug query COMMIT | START",
		"rollback":                             "debug query ROLLBACK carts | START",
	} {
		if got := lastLineAt[call]; got != want {
			t.Errorf("last line when the driver got %q = %q, want %q", call, got, want)
		}
	}
}

// taggedContext cannot be compared with ==, because it holds a map.
type taggedContext struct {
	context.Context
	tags map[string]string
}

// A context whose type cannot be compared must neither panic nor lose the
// lines of a statement the driver skips.
func TestConnectorMatchesASkippedStatementWhoseContextCannotBeCompared(t *testing.T) {
	backend := newFakeBackend()
	backend.skipWithArgs = true
	db, sink, _ := openLoggedDB(t, backend, false)
	ctx := taggedContext{Context: context.Background(), tags: map[string]string{"cart": "7"}}

	if _, err := db.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", 7); err != nil {
		t.Fatal(err)
	}

	assertShapes(t, sink,
		"debug query DELETE carts | START",
		"debug query DELETE carts | DONE Nms",
	)
}

// Without a driver.NamedValueChecker on inner, database/sql must reject the
// arguments it rejects for the driver alone, before any line is written.
func TestConnectorRejectsArgumentsAsDatabaseSQLDoesWithoutAChecker(t *testing.T) {
	backend := newFakeBackend()
	plain := sql.OpenDB(&fakeConnector{backend: backend})
	defer plain.Close()
	db, sink, _ := openLoggedDB(t, backend, false)
	ctx := context.Background()

	_, plainErr := plain.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", cartID{7})
	_, err := db.ExecContext(ctx, "DELETE FROM carts WHERE id = $1", cartID{7})

	if plainErr == nil {
		t.Fatal("the driver alone accepted cartID")
	}
	if err == nil || err.Error() != plainErr.Error() {
		t.Fatalf("err = %v, want %v", err, plainErr)
	}
	assertShapes(t, sink)
}
