package spanlog

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

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
