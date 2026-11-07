package spanlog

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type transactionCase struct {
	Name       string     `json:"name"`
	Statements []string   `json:"statements"`
	Rollbacks  [][]string `json:"rollbacks"`
}

func loadTransactionCases(t *testing.T) []transactionCase {
	t.Helper()
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var cases struct {
		Transactions []transactionCase `json:"transactions"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode shared cases: %v", err)
	}
	return cases.Transactions
}

// rollbacksOf runs statements the way the connection wrapper does,
// statementStarting before each one and statementDone after it.
func rollbacksOf(statements []string) [][]string {
	var transaction transactionTables
	rollbacks := [][]string{}
	for _, sql := range statements {
		if undone, isRollback := transaction.statementStarting(sql); isRollback {
			rollbacks = append(rollbacks, undone)
		}
		transaction.statementDone(sql)
	}
	return rollbacks
}

func TestGoldenTransactionCases(t *testing.T) {
	cases := loadTransactionCases(t)
	if len(cases) < 25 {
		t.Fatalf("loaded %d transaction cases, want at least 25", len(cases))
	}
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			got := rollbacksOf(testCase.Statements)
			if !reflect.DeepEqual(got, testCase.Rollbacks) {
				t.Errorf("rollbacks = %q, want %q", got, testCase.Rollbacks)
			}
		})
	}
}

// A failed statement gets statementStarting and no statementDone, as the
// wrapper calls statementDone only after a statement without an error.
func TestTransactionTablesDropAFailedWrite(t *testing.T) {
	var transaction transactionTables
	transaction.statementStarting("BEGIN")
	transaction.statementDone("BEGIN")
	transaction.statementStarting("INSERT INTO a (v) VALUES ('x')")

	undone, _ := transaction.statementStarting("ROLLBACK")

	if !reflect.DeepEqual(undone, []string{}) {
		t.Errorf("undone = %q", undone)
	}
}

func TestTransactionTablesOpenNothingAfterAFailedCommitAndChain(t *testing.T) {
	var transaction transactionTables
	for _, sql := range []string{"BEGIN", "INSERT INTO a (v) VALUES (1)"} {
		transaction.statementStarting(sql)
		transaction.statementDone(sql)
	}
	transaction.statementStarting("COMMIT AND CHAIN")
	transaction.statementStarting("INSERT INTO b (v) VALUES (1)")
	transaction.statementDone("INSERT INTO b (v) VALUES (1)")

	undone, _ := transaction.statementStarting("ROLLBACK")

	if !reflect.DeepEqual(undone, []string{}) {
		t.Errorf("undone = %q", undone)
	}
}

// database/sql runs BEGIN, COMMIT and ROLLBACK of a Tx through the driver with
// no statement.
func TestTransactionTablesFollowATransactionWithoutStatements(t *testing.T) {
	var transaction transactionTables
	transaction.begin()
	transaction.statementStarting(`UPDATE "warehouse"."inventory" SET available = $1`)
	transaction.statementDone(`UPDATE "warehouse"."inventory" SET available = $1`)

	if got := transaction.rollBack(); !reflect.DeepEqual(got, []string{"warehouse.inventory"}) {
		t.Errorf("first rollback = %q", got)
	}
	if got := transaction.rollBack(); !reflect.DeepEqual(got, []string{}) {
		t.Errorf("second rollback = %q", got)
	}
}
