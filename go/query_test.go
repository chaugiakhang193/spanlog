package spanlog

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// inFrame runs fn inside OrderService.Place of request r1 and returns its error.
func inFrame(log *Logger, fn func(ctx context.Context) error) (err error) {
	ctx, done := log.Start(WithRequest(context.Background(), "r1"), "OrderService", "Place")
	defer done(&err)
	return fn(ctx)
}

func TestStartQueryWritesTheOpAndTablesOfTheStatement(t *testing.T) {
	log, sink := newTestLogger(Options{})

	span := log.StartQuery(context.Background(), `UPDATE "warehouse"."inventory" SET available = $1 WHERE sku = 'secret'`)
	span.End()

	assertShapes(t, sink,
		"debug query UPDATE warehouse.inventory | START",
		"debug query UPDATE warehouse.inventory | DONE Nms",
	)
	fields := sink.seen[0].Fields
	if fields.Op != "UPDATE" || !reflect.DeepEqual(fields.Tables, []string{"warehouse.inventory"}) {
		t.Errorf("op = %q, tables = %q", fields.Op, fields.Tables)
	}
	for _, rendered := range sink.seen {
		if strings.Contains(strings.Join(rendered.Lines, "\n"), "secret") {
			t.Errorf("line holds a value of the statement: %q", rendered.Lines)
		}
	}
}

func TestStartQueryBelongsToTheFrameOfTheContext(t *testing.T) {
	log, sink := newTestLogger(Options{})

	_ = inFrame(log, func(ctx context.Context) error {
		log.StartQuery(ctx, "SELECT 1 FROM orders").End()
		return nil
	})

	assertShapes(t, sink,
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query SELECT orders | START",
		"debug OrderService.Place - query SELECT orders | DONE Nms",
		"info OrderService.Place - run | DONE Nms",
	)
	if got := sink.seen[1].Fields.ReqID; got != "r1" {
		t.Errorf("reqId = %q", got)
	}
}

func TestStartQueryNamesTheTablesARollbackUndoes(t *testing.T) {
	log, sink := newTestLogger(Options{})

	log.StartQuery(context.Background(), "ROLLBACK", WithRolledBackTables([]string{"orders"})).End()
	log.StartQuery(context.Background(), "SELECT 1 FROM items", WithRolledBackTables([]string{"orders"})).End()
	log.StartQuery(context.Background(), "ROLLBACK").End()

	assertShapes(t, sink,
		"debug query ROLLBACK orders | START",
		"warn query ROLLBACK orders | DONE Nms",
		"debug query SELECT items | START",
		"debug query SELECT items | DONE Nms",
		"debug query ROLLBACK | START",
		"warn query ROLLBACK | DONE Nms",
	)
}

func TestQuerySpanFailWritesTheMessageWithoutAStackAndLeavesTheErrorUnreported(t *testing.T) {
	log, sink := newTestLogger(Options{})
	failure := errors.New("duplicate key")

	err := inFrame(log, func(ctx context.Context) error {
		span := log.StartQuery(ctx, "INSERT INTO orders (id) VALUES ($1)")
		span.Fail(failure)
		span.End()
		return failure
	})

	if err != failure {
		t.Fatalf("err = %v", err)
	}
	assertShapes(t, sink,
		"info OrderService.Place - run | START",
		"debug OrderService.Place - query INSERT orders | START",
		"warn OrderService.Place - query INSERT orders | FAILED Nms duplicate key",
		"error OrderService.Place - run | FAILED Nms duplicate key",
	)
	if stack := sink.seen[2].Fields.Err.Stack; stack != "" {
		t.Errorf("query line has a stack: %q", stack)
	}
	if sink.seen[3].Fields.Err.Stack == "" {
		t.Errorf("run line has no stack")
	}
}

func TestQuerySpanFailWithNilEndsWithDone(t *testing.T) {
	log, sink := newTestLogger(Options{})

	log.StartQuery(context.Background(), "SELECT 1").Fail(nil)

	assertShapes(t, sink, "debug query SELECT | START", "debug query SELECT | DONE Nms")
}

func TestSlowQueryEndsWithSlow(t *testing.T) {
	slowMs := 1.0
	log, sink := newTestLogger(Options{SlowMs: &slowMs})

	span := log.StartQuery(context.Background(), "SELECT 1 FROM orders")
	time.Sleep(5 * time.Millisecond)
	span.End()

	assertShapes(t, sink, "debug query SELECT orders | START", "warn query SELECT orders | SLOW Nms")
}
