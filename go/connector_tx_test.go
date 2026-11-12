package spanlog

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

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
