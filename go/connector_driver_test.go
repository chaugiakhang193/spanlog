package spanlog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
)

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
