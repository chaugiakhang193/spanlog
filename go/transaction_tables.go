package spanlog

import "slices"

var beginOps = map[string]bool{"BEGIN": true, "START TRANSACTION": true}

// ABORT is a ROLLBACK in Postgres. Its line keeps the op ABORT. That line stays
// at debug and names no table.
var endOps = map[string]bool{"COMMIT": true, "END": true, "ABORT": true}

// A write inside a common table expression of a SELECT is not seen, because the
// op of that statement is SELECT.
var writeOps = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true}

// transactionTables follows the statements of one connection and keeps the
// tables that its open transaction has written. A ROLLBACK line names them. The
// rules are those of the query lines in docs/log-format.md. begin, end and
// rollBack serve a transaction that the driver runs without a statement.
type transactionTables struct {
	// levels[0] holds the writes of the transaction, and each later entry those
	// of one savepoint, innermost last. ROLLBACK TO and RELEASE act on the
	// innermost savepoint, whatever name they give.
	levels [][]string
}

// statementStarting runs before a statement. For a ROLLBACK it returns the
// tables that the rollback undoes and true. A statement that ends the
// transaction ends it here, because Postgres also ends a transaction whose
// COMMIT fails.
func (t *transactionTables) statementStarting(sql string) ([]string, bool) {
	op := summarizeSQL(sql).Op
	switch {
	case beginOps[op]:
		t.begin()
	case endOps[op]:
		t.end()
	case op == "SAVEPOINT":
		t.openSavepoint()
	case op == "RELEASE":
		t.releaseSavepoint()
	case op == "ROLLBACK":
		if rollsBackToSavepoint(sql) {
			return t.rollBackToSavepoint(), true
		}
		return t.rollBack(), true
	}
	return nil, false
}

// statementDone runs after a statement that ended without an error. A statement
// with AND CHAIN opens the next transaction here, because Postgres opens it only
// when that statement succeeds.
func (t *transactionTables) statementDone(sql string) {
	summary := summarizeSQL(sql)
	switch {
	case writeOps[summary.Op] && len(t.levels) > 0:
		innermost := len(t.levels) - 1
		t.levels[innermost] = addNew(t.levels[innermost], summary.Tables)
	case (endOps[summary.Op] || summary.Op == "ROLLBACK") && chainsTransaction(sql):
		t.begin()
	}
}

func (t *transactionTables) begin() {
	t.levels = [][]string{{}}
}

func (t *transactionTables) end() {
	t.levels = nil
}

func (t *transactionTables) rollBack() []string {
	undone := []string{}
	for _, level := range t.levels {
		undone = addNew(undone, level)
	}
	t.levels = nil
	return undone
}

// Postgres keeps the savepoint open after ROLLBACK TO, with nothing written
// since.
func (t *transactionTables) rollBackToSavepoint() []string {
	if len(t.levels) < 2 {
		return []string{}
	}
	innermost := len(t.levels) - 1
	undone := t.levels[innermost]
	t.levels[innermost] = []string{}
	return undone
}

func (t *transactionTables) openSavepoint() {
	if len(t.levels) > 0 {
		t.levels = append(t.levels, []string{})
	}
}

func (t *transactionTables) releaseSavepoint() {
	if len(t.levels) < 2 {
		return
	}
	released := t.levels[len(t.levels)-1]
	t.levels = t.levels[:len(t.levels)-1]
	outer := len(t.levels) - 1
	t.levels[outer] = addNew(t.levels[outer], released)
}

func addNew(into, tables []string) []string {
	for _, table := range tables {
		if !slices.Contains(into, table) {
			into = append(into, table)
		}
	}
	return into
}
