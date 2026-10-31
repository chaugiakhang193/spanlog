package spanlog

import (
	"math"
	"reflect"
	"testing"
)

func TestRedactedNegativeZeroEncodesWithoutASign(t *testing.T) {
	got, err := Redact(map[string]any{"value": math.Copysign(0, -1)}, RedactOptions{})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := encodeJSON(got)

	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"value":0}` {
		t.Errorf("got %s", encoded)
	}
}

func TestRedactMasksKeysAtAnyDepthWithoutRegardToCase(t *testing.T) {
	input := map[string]any{
		"Password": "p",
		"order":    map[string]any{"id": 1, "items": []any{map[string]any{"TOKEN": "t"}}},
	}

	got, err := Redact(input, RedactOptions{})

	if err != nil {
		t.Fatal(err)
	}
	if stableJSON(got) != `{"Password":"[REDACTED]","order":{"id":1,"items":[{"TOKEN":"[REDACTED]"}]}}` {
		t.Errorf("got %s", stableJSON(got))
	}
	if input["Password"] != "p" {
		t.Error("input was modified")
	}
}

func TestRedactAddsConfiguredKeys(t *testing.T) {
	got, err := Redact(map[string]any{"pin": 1}, RedactOptions{Keys: []string{"PIN"}})

	if err != nil {
		t.Fatal(err)
	}
	if stableJSON(got) != `{"pin":"[REDACTED]"}` {
		t.Errorf("got %s", stableJSON(got))
	}
}

func TestRedactReadsStructsThroughTheirJSONForm(t *testing.T) {
	type account struct {
		Name     string
		Password string
		Internal string `json:"-"`
		Mail     string `json:"email"`
	}

	got, err := Redact(account{Name: "a", Password: "p", Internal: "i", Mail: "m"}, RedactOptions{})

	if err != nil {
		t.Fatal(err)
	}
	if stableJSON(got) != `{"Name":"a","Password":"[REDACTED]","email":"[REDACTED]"}` {
		t.Errorf("got %s", stableJSON(got))
	}
}

func nested(levels int) any {
	var value any = "leaf"
	for range levels {
		value = map[string]any{"a": value}
	}
	return value
}

func TestRedactTruncatesBelowTheDepthLimit(t *testing.T) {
	got, err := Redact(nested(9), RedactOptions{})

	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":"[TRUNCATED]"}}}}}}}}`
	if stableJSON(got) != want {
		t.Errorf("got %s, want %s", stableJSON(got), want)
	}

	exact, err := Redact(nested(8), RedactOptions{})

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exact, nested(8)) {
		t.Errorf("value at the limit changed: %s", stableJSON(exact))
	}
}

func TestRedactFallsBackToTheDefaultDepthForANegativeLimit(t *testing.T) {
	limit := -1

	got, err := Redact(nested(8), RedactOptions{MaxDepth: &limit})

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, nested(8)) {
		t.Errorf("got %s", stableJSON(got))
	}
}

func TestRedactReturnsAnErrorForAValueJSONCannotEncode(t *testing.T) {
	type node struct{ Next *node }
	cycle := &node{}
	cycle.Next = cycle

	for name, value := range map[string]any{"channel": make(chan int), "cycle": cycle} {
		t.Run(name, func(t *testing.T) {
			if _, err := Redact(value, RedactOptions{}); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestStableJSONSortsAndMatchesTheNodeEncoding(t *testing.T) {
	type pair struct {
		Z int `json:"z"`
		A int `json:"a"`
	}
	cases := map[string]struct {
		value any
		want  string
	}{
		"map keys":          {map[string]any{"b": 1, "a": map[string]any{"d": 1, "c": 2}}, `{"a":{"c":2,"d":1},"b":1}`},
		"struct fields":     {pair{Z: 1, A: 2}, `{"a":2,"z":1}`},
		"html characters":   {"<&>", `"<&>"`},
		"line separators":   {string(rune(0x2028)) + string(rune(0x2029)), `"` + string(rune(0x2028)) + string(rune(0x2029)) + `"`},
		"escaped backslash": {`\` + `u2028`, `"\\` + `u2028"`},
		"large integer":     {uint64(18446744073709551615), `18446744073709551615`},
		"utf-16 key order":  {map[string]any{string(rune(0xE000)): 1, string(rune(0x10000)): 2}, `{"` + string(rune(0x10000)) + `":2,"` + string(rune(0xE000)) + `":1}`},
		"encoding failure":  {make(chan int), `null`},
		"negative zero":     {map[string]any{"a": []any{math.Copysign(0, -1)}}, `{"a":[0]}`},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := stableJSON(testCase.value); got != testCase.want {
				t.Errorf("got %s, want %s", got, testCase.want)
			}
		})
	}
}
