// Package pgtest runs the database/sql connector of spanlog against Postgres
// through pgx. It is a module of its own, so the spanlog module keeps no
// dependency outside the standard library.
package pgtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaugiakhang193/spanlog/go"
	"github.com/jackc/pgx/v5/stdlib"
)

var measured = regexp.MustCompile(`\d+(\.\d+)?ms`)

type recordingSink struct {
	mu   sync.Mutex
	seen []spanlog.Rendered
}

func (s *recordingSink) Write(rendered spanlog.Rendered) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, rendered)
	return nil
}

func (s *recordingSink) shapes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.seen))
	for index, rendered := range s.seen {
		out[index] = string(rendered.Level) + " " + measured.ReplaceAllString(rendered.Msg, "Nms")
	}
	return out
}

func (s *recordingSink) last() spanlog.Rendered {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[len(s.seen)-1]
}

func databaseURL() string {
	if url := os.Getenv("SPANLOG_PG_URL"); url != "" {
		return url
	}
	return "postgres://spanlog:spanlog@127.0.0.1:5438/spanlog"
}

func connector(t *testing.T) driver.Connector {
	t.Helper()
	connector, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(databaseURL())
	if err != nil {
		t.Fatal(err)
	}
	return connector
}

// openDB returns a database whose statements write lines into the returned
// sink, and creates the tables of the tests in a schema that it drops at the
// end.
func openDB(t *testing.T) (*sql.DB, *recordingSink, *spanlog.Logger) {
	t.Helper()
	sink := &recordingSink{}
	log := spanlog.New(spanlog.Options{Service: "shop", Sink: sink, Level: spanlog.LevelDebug})
	db := sql.OpenDB(log.Connector(connector(t)))
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("Postgres at %s: %v. Start it with docker compose up -d --wait", databaseURL(), err)
	}
	setup := []string{
		"DROP SCHEMA IF EXISTS gotest CASCADE",
		"CREATE SCHEMA gotest",
		"CREATE TABLE gotest.orders (id int PRIMARY KEY, sku text NOT NULL)",
		"CREATE TABLE gotest.inventory (sku text PRIMARY KEY, available int NOT NULL CHECK (available >= 0))",
		"INSERT INTO gotest.inventory VALUES ('apple', 1)",
	}
	for _, statement := range setup {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), "DROP SCHEMA gotest CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	sink.mu.Lock()
	sink.seen = nil
	sink.mu.Unlock()
	return db, sink, log
}

func inFrame(log *spanlog.Logger, fn func(ctx context.Context) error) (err error) {
	ctx, done := log.Start(spanlog.WithRequest(context.Background(), "r1"), "OrderService", "Place")
	defer done(&err)
	return fn(ctx)
}

func count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPostgresWritesTheLinesOfAStatement(t *testing.T) {
	db, sink, log := openDB(t)

	err := inFrame(log, func(ctx context.Context) error {
		_, err := db.ExecContext(ctx, `INSERT INTO "gotest"."orders" ("id", "sku") VALUES ($1, $2)`, 1, "apple")
		return err
	})

	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query INSERT gotest.orders | START",
		"debug OrderService.Place - query INSERT gotest.orders | DONE Nms",
		"info OrderService.Place - run | DONE Nms",
	}
	if got := sink.shapes(); !reflect.DeepEqual(got, want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
}

// Postgres sends the error of the third row after the first two rows.
func TestPostgresFailsAQueryWhenReadingARowFails(t *testing.T) {
	db, sink, _ := openDB(t)

	rows, err := db.QueryContext(context.Background(), "SELECT 1 / (n - 3) FROM generate_series(1, 5) n")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for rows.Next() {
		read++
	}

	if rows.Err() == nil || read != 2 {
		t.Fatalf("read %d rows, rows.Err() = %v", read, rows.Err())
	}
	want := []string{
		"debug query SELECT generate_series | START",
		"warn query SELECT generate_series | FAILED Nms ERROR: division by zero (SQLSTATE 22012)",
	}
	if got := sink.shapes(); !reflect.DeepEqual(got, want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
}

// waitForLines returns the lines of sink once it holds n of them, or fails the
// test after five seconds.
func waitForLines(t *testing.T, sink *recordingSink, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.shapes()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := sink.shapes()
	if len(got) < n {
		t.Fatalf("want %d lines, got %d:\n%s", n, len(got), strings.Join(got, "\n"))
	}
	return got
}

// The context ends while the rows are still open. database/sql closes the rows
// on a goroutine of its own when the context ends, and keeps the error of the
// context for rows.Err(). pgx reacts to the same cancellation at the same
// time. The end line is DONE when the rows close first and FAILED with the
// error of pgx when pgx sees the cancellation first. The test waits for the
// end line written from that goroutine. A Close from the test before that
// goroutine runs leaves rows.Err() without the error of the context.
func TestPostgresWritesTheEndOfAQueryWhoseContextEndsWhileItsRowsAreRead(t *testing.T) {
	db, sink, _ := openDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rows, err := db.QueryContext(ctx, "SELECT n FROM generate_series(1, 1000) n")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("first row: %v", rows.Err())
	}
	cancel()

	got := waitForLines(t, sink, 2)
	ends := []string{
		"debug query SELECT generate_series | DONE Nms",
		"warn query SELECT generate_series | FAILED Nms context canceled",
	}
	if len(got) != 2 || got[0] != "debug query SELECT generate_series | START" || !slices.Contains(ends, got[1]) {
		t.Errorf("lines:\n%s\nrows.Err() = %v", strings.Join(got, "\n"), rows.Err())
	}
	t.Logf("end line: %s", got[len(got)-1])
	if !errors.Is(rows.Err(), context.Canceled) {
		t.Errorf("rows.Err() after cancel = %v", rows.Err())
	}
}

func TestPostgresNamesTheTablesARolledBackTransactionWrote(t *testing.T) {
	db, sink, log := openDB(t)

	err := inFrame(log, func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "INSERT INTO gotest.orders (id, sku) VALUES ($1, $2)", 1, "apple"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE gotest.inventory SET available = available - 2 WHERE sku = $1", "apple")
		return err
	})

	if err == nil {
		t.Fatal("the check constraint did not fail")
	}
	want := []string{
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query BEGIN | START",
		"debug OrderService.Place - query BEGIN | DONE Nms",
		"debug OrderService.Place - query INSERT gotest.orders | START",
		"debug OrderService.Place - query INSERT gotest.orders | DONE Nms",
		"debug OrderService.Place - query UPDATE gotest.inventory | START",
		`warn OrderService.Place - query UPDATE gotest.inventory | FAILED Nms ERROR: new row for relation "inventory" violates check constraint "inventory_available_check" (SQLSTATE 23514)`,
		"debug OrderService.Place - query ROLLBACK gotest.orders | START",
		"warn OrderService.Place - query ROLLBACK gotest.orders | DONE Nms",
		`error OrderService.Place - run | FAILED Nms ERROR: new row for relation "inventory" violates check constraint "inventory_available_check" (SQLSTATE 23514)`,
	}
	if got := sink.shapes(); !reflect.DeepEqual(got, want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
	if n := count(t, db, "SELECT count(*) FROM gotest.orders"); n != 0 {
		t.Errorf("orders after rollback = %d", n)
	}
}

func TestPostgresNamesTheTablesWrittenSinceASavepoint(t *testing.T) {
	db, sink, log := openDB(t)
	statements := []string{
		"INSERT INTO gotest.orders (id, sku) VALUES (1, 'apple')",
		"SAVEPOINT reserve",
		"UPDATE gotest.inventory SET available = 0 WHERE sku = 'apple'",
		"ROLLBACK TO SAVEPOINT reserve",
	}

	err := inFrame(log, func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return tx.Commit()
	})

	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query BEGIN | START",
		"debug OrderService.Place - query BEGIN | DONE Nms",
		"debug OrderService.Place - query INSERT gotest.orders | START",
		"debug OrderService.Place - query INSERT gotest.orders | DONE Nms",
		"debug OrderService.Place - query SAVEPOINT | START",
		"debug OrderService.Place - query SAVEPOINT | DONE Nms",
		"debug OrderService.Place - query UPDATE gotest.inventory | START",
		"debug OrderService.Place - query UPDATE gotest.inventory | DONE Nms",
		"debug OrderService.Place - query ROLLBACK gotest.inventory | START",
		"warn OrderService.Place - query ROLLBACK gotest.inventory | DONE Nms",
		"debug OrderService.Place - query COMMIT | START",
		"debug OrderService.Place - query COMMIT | DONE Nms",
		"info OrderService.Place - run | DONE Nms",
	}
	if got := sink.shapes(); !reflect.DeepEqual(got, want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
	if n := count(t, db, "SELECT available FROM gotest.inventory WHERE sku = 'apple'"); n != 1 {
		t.Errorf("available after rollback to savepoint = %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM gotest.orders"); n != 1 {
		t.Errorf("orders after commit = %d", n)
	}
}

// database/sql rolls the transaction back when its context ends. pgx closes a
// connection whose statement is cancelled, so the ROLLBACK fails with conn
// closed, and Postgres undoes the transaction as the session ends.
func TestPostgresWritesTheRollbackOfACancelledTransaction(t *testing.T) {
	db, sink, log := openDB(t)
	want := "warn OrderService.Place - query ROLLBACK gotest.orders | FAILED Nms conn closed"

	_ = inFrame(log, func(ctx context.Context) error {
		txContext, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		tx, err := db.BeginTx(txContext, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(txContext, "INSERT INTO gotest.orders (id, sku) VALUES (1, 'apple')"); err != nil {
			return err
		}
		_, err = tx.ExecContext(txContext, "SELECT pg_sleep(5)")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("pg_sleep err = %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !slices.Contains(sink.shapes(), want) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return err
	})

	got := sink.shapes()
	if !slices.Contains(got, want) {
		t.Errorf("lines:\n%s\nwant a line %q", strings.Join(got, "\n"), want)
	}
	if n := count(t, db, "SELECT count(*) FROM gotest.orders"); n != 0 {
		t.Errorf("orders after rollback = %d", n)
	}
}

func TestPostgresKeepsTheColumnTypesOfPgx(t *testing.T) {
	db, _, _ := openDB(t)
	plain := sql.OpenDB(connector(t))
	defer plain.Close()
	query := "SELECT 1::int4 AS a, 'x'::varchar(8) AS b, 1.5::numeric(5, 2) AS c"

	if got, want := columnTypesOf(t, db, query), columnTypesOf(t, plain, query); !reflect.DeepEqual(got, want) {
		t.Errorf("logged %v, plain %v", got, want)
	}
}

func columnTypesOf(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for _, column := range types {
		length, hasLength := column.Length()
		nullable, hasNullable := column.Nullable()
		precision, scale, hasPrecision := column.DecimalSize()
		out = append(out, []any{column.Name(), column.ScanType(), column.DatabaseTypeName(), length, hasLength,
			nullable, hasNullable, precision, scale, hasPrecision})
	}
	return out
}
