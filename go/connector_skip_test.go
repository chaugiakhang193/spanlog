package spanlog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

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
