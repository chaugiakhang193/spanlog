package spanlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

type goldenEvent struct {
	Time        time.Time      `json:"time"`
	Service     string         `json:"service"`
	ReqID       string         `json:"reqId"`
	Scope       string         `json:"scope"`
	Method      string         `json:"method"`
	Action      Action         `json:"action"`
	Target      string         `json:"target"`
	SameScope   bool           `json:"sameScope"`
	Phase       Phase          `json:"phase"`
	DurationMs  *float64       `json:"durationMs"`
	Status      *int           `json:"status"`
	Op          string         `json:"op"`
	Tables      []string       `json:"tables"`
	Err         *ErrorInfo     `json:"err"`
	FirstReport bool           `json:"firstReport"`
	Params      map[string]any `json:"params"`
	Summary     map[string]any `json:"summary"`
}

type goldenCase struct {
	Name    string `json:"name"`
	Options struct {
		UTCOffsetMinutes int `json:"utcOffsetMinutes"`
	} `json:"options"`
	Event  goldenEvent `json:"event"`
	Expect struct {
		Level  Level          `json:"level"`
		Msg    string         `json:"msg"`
		Fields map[string]any `json:"fields"`
		Lines  []string       `json:"lines"`
	} `json:"expect"`
}

// testdata/cases.json travels with the module, whose zip holds only go/. Inside the
// repository it must equal the shared file.
func TestSharedCasesMatchTheSpec(t *testing.T) {
	shared, err := os.ReadFile("../spec/cases.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("outside the repository")
	}
	if err != nil {
		t.Fatal(err)
	}
	local, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(shared, local) {
		t.Error("testdata/cases.json differs from spec/cases.json, copy the file")
	}
}

func loadRenderCases(t *testing.T) []goldenCase {
	t.Helper()
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var cases struct {
		Render []goldenCase `json:"render"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode shared cases: %v", err)
	}
	return cases.Render
}

func fieldsAsMap(t *testing.T, fields Fields) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode fields: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	return decoded
}

func TestGoldenRenderCases(t *testing.T) {
	cases := loadRenderCases(t)
	if len(cases) < 25 {
		t.Fatalf("loaded %d render cases, want at least 25", len(cases))
	}
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			event := Event(testCase.Event)
			rendered := Render(event, RenderOptions{UTCOffsetMinutes: testCase.Options.UTCOffsetMinutes})

			if rendered.Level != testCase.Expect.Level {
				t.Errorf("level = %q, want %q", rendered.Level, testCase.Expect.Level)
			}
			if rendered.Msg != testCase.Expect.Msg {
				t.Errorf("msg = %q, want %q", rendered.Msg, testCase.Expect.Msg)
			}
			if !reflect.DeepEqual(rendered.Lines, testCase.Expect.Lines) {
				t.Errorf("lines = %q, want %q", rendered.Lines, testCase.Expect.Lines)
			}
			fields := fieldsAsMap(t, rendered.Fields)
			if !reflect.DeepEqual(fields, testCase.Expect.Fields) {
				t.Errorf("fields = %v, want %v", fields, testCase.Expect.Fields)
			}
		})
	}
}

func TestSmallNegativeDurationHasNoMinusSign(t *testing.T) {
	ms := -0.04

	rendered := Render(Event{Service: "shop", Action: ActionRun, Phase: PhaseDone, DurationMs: &ms}, RenderOptions{})

	if rendered.Msg != "run | DONE 0ms" {
		t.Errorf("msg = %q", rendered.Msg)
	}
}

func TestRenderKeepsFieldOrderOfTheSpec(t *testing.T) {
	ms := 3.04
	status := 500
	rendered := Render(Event{
		Service:    "shop",
		ReqID:      "r",
		Action:     ActionRequest,
		Target:     "POST /orders",
		Phase:      PhaseFailed,
		DurationMs: &ms,
		Status:     &status,
	}, RenderOptions{})

	encoded, err := encodeJSON(rendered.Fields)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"service":"shop","reqId":"r","action":"request","target":"POST /orders","phase":"FAILED","ms":3,"status":500}`
	if string(encoded) != want {
		t.Errorf("got %s, want %s", encoded, want)
	}
}
