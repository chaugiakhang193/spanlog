package spanlog

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sqlCase struct {
	Name   string   `json:"name"`
	SQL    string   `json:"sql"`
	Op     string   `json:"op"`
	Tables []string `json:"tables"`
}

func loadSQLCases(t *testing.T) []sqlCase {
	t.Helper()
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var cases struct {
		SQL []sqlCase `json:"sql"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode shared cases: %v", err)
	}
	return cases.SQL
}

func TestGoldenSQLCases(t *testing.T) {
	cases := loadSQLCases(t)
	if len(cases) < 75 {
		t.Fatalf("loaded %d sql cases, want at least 75", len(cases))
	}
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			got := summarizeSQL(testCase.SQL)
			want := sqlSummary{Op: testCase.Op, Tables: testCase.Tables}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("summarizeSQL = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSummarizeSQLKeepsADoubledQuoteInsideAQuotedName(t *testing.T) {
	got := summarizeSQL(`SELECT 1 FROM "odd""name"`).Tables
	if !reflect.DeepEqual(got, []string{`odd"name`}) {
		t.Errorf("tables = %q", got)
	}
}

func TestSummarizeSQLListsATableOnce(t *testing.T) {
	got := summarizeSQL("SELECT a.id FROM orders a JOIN orders b ON b.parent_id = a.id").Tables
	if !reflect.DeepEqual(got, []string{"orders"}) {
		t.Errorf("tables = %q", got)
	}
}

func TestSummarizeSQLOfTextThatIsNotAStatement(t *testing.T) {
	for _, sql := range []string{"", "  $1 "} {
		got := summarizeSQL(sql)
		if !reflect.DeepEqual(got, sqlSummary{Op: "", Tables: []string{}}) {
			t.Errorf("summarizeSQL(%q) = %+v", sql, got)
		}
	}
}

func TestSummarizeSQLSurvivesUnclosedText(t *testing.T) {
	if got := summarizeSQL(`SELECT * FROM "orders`); !reflect.DeepEqual(got, sqlSummary{Op: "SELECT", Tables: []string{"orders"}}) {
		t.Errorf("unclosed quoted name: %+v", got)
	}
	if got := summarizeSQL("WITH x AS (SELECT 1").Op; got != "" {
		t.Errorf("unclosed parenthesis: op = %q", got)
	}
	for _, sql := range []string{
		"SELECT 'from alice FROM orders",
		"SELECT /* from alice FROM orders",
		"SELECT $$ from alice FROM orders",
		"SELECT E'from alice FROM orders",
	} {
		if got := summarizeSQL(sql).Tables; len(got) != 0 {
			t.Errorf("summarizeSQL(%q).Tables = %q", sql, got)
		}
	}
}

func TestSummarizeSQLKeepsANameThatHoldsADollarSign(t *testing.T) {
	got := summarizeSQL("SELECT 1 FROM a$b$c").Tables
	if !reflect.DeepEqual(got, []string{"a$b$c"}) {
		t.Errorf("tables = %q", got)
	}
}

func TestSummarizeSQLDoesNotReadAQuotedNameAsAParenthesis(t *testing.T) {
	got := summarizeSQL(`WITH a AS (SELECT "(" FROM items) SELECT * FROM a`)
	if !reflect.DeepEqual(got, sqlSummary{Op: "SELECT", Tables: []string{"items"}}) {
		t.Errorf("summarizeSQL = %+v", got)
	}
}

// A string holds bytes that are not UTF-8 when the driver sends them as they
// came. Each such byte reads as a letter, as a character from U+0080 up does.
func TestSummarizeSQLReadsBytesThatAreNotUTF8AsLetters(t *testing.T) {
	got := summarizeSQL("SELECT 1 FROM t\xff\xfe")
	if !reflect.DeepEqual(got.Tables, []string{"t\xff\xfe"}) {
		t.Errorf("tables = %q", got.Tables)
	}
}

// A scan of every scope for each name takes over 10 s at this depth with one
// name. The limit leaves room for a loaded machine.
func TestSummarizeSQLReadsDeeplyNestedCommonTableExpressions(t *testing.T) {
	for label, name := range map[string]func(int) string{
		"different names": func(level int) string { return fmt.Sprintf("c%d", level) },
		"one name":        func(int) string { return "c" },
	} {
		t.Run(label, func(t *testing.T) {
			sql := nestedExpressions(128_000, name)

			start := time.Now()
			got := summarizeSQL(sql).Tables
			elapsed := time.Since(start)

			if !reflect.DeepEqual(got, []string{"orders"}) {
				t.Errorf("tables = %q", got)
			}
			if elapsed > 5*time.Second {
				t.Errorf("took %v", elapsed)
			}
		})
	}
}

// nestedExpressions wraps SELECT * FROM orders in levels of WITH name AS (...)
// SELECT * FROM name, level 0 innermost. A Builder writes the text once. Joining
// strings level by level copies the whole text at each level.
func nestedExpressions(levels int, name func(int) string) string {
	var sql strings.Builder
	for level := levels - 1; level >= 0; level-- {
		sql.WriteString("WITH " + name(level) + " AS (")
	}
	sql.WriteString("SELECT * FROM orders")
	for level := range levels {
		sql.WriteString(") SELECT * FROM " + name(level))
	}
	return sql.String()
}

// A scan of the tables seen so far for each name takes over 30 s at this count.
func TestSummarizeSQLReadsAStatementThatNamesManyTables(t *testing.T) {
	names := make([]string, 128_000)
	for index := range names {
		names[index] = fmt.Sprintf("t%d", index)
	}
	sql := "SELECT * FROM " + strings.Join(names, " JOIN ")

	start := time.Now()
	got := summarizeSQL(sql).Tables
	elapsed := time.Since(start)

	if !reflect.DeepEqual(got, names) {
		t.Errorf("got %d tables, want %d", len(got), len(names))
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v", elapsed)
	}
}

func TestQueryTarget(t *testing.T) {
	if got := queryTarget(sqlSummary{Op: "SELECT", Tables: []string{"orders", "items"}}); got != "SELECT orders, items" {
		t.Errorf("with tables: %q", got)
	}
	if got := queryTarget(sqlSummary{Op: "COMMIT", Tables: []string{}}); got != "COMMIT" {
		t.Errorf("without tables: %q", got)
	}
}
