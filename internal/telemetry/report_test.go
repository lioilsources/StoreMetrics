package telemetry

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type fakeQuerier struct{ calls []string }

func (f *fakeQuerier) Query(_ context.Context, sql string, args ...any) ([]string, [][]any, error) {
	f.calls = append(f.calls, sql)
	if len(args) != 2 || args[0] != "lexify" {
		return nil, nil, nil
	}
	return []string{"obrazovka", "počet"}, [][]any{{"deck", int64(3)}, {"a|b", 1.25}}, nil
}

func TestReportCoversAllQuestions(t *testing.T) {
	if len(Sections) != 8 {
		t.Fatalf("%d sections, want the 8 questions of §1", len(Sections))
	}
	for _, s := range Sections {
		for _, tb := range s.Tables {
			if !strings.Contains(tb.SQL, "$1") || !strings.Contains(tb.SQL, "$2") {
				t.Errorf("%s / %s: query must use $1 (app) and $2 (since)", s.Title, tb.Title)
			}
			if strings.Contains(tb.SQL, "telemetry.events") && !strings.Contains(tb.SQL, `NOT props @> '{"tester": true}'`) {
				t.Errorf("%s / %s: tester builds not excluded", s.Title, tb.Title)
			}
		}
	}
	q := &fakeQuerier{}
	var out bytes.Buffer
	since := testNow.AddDate(0, 0, -7)
	if err := Report(context.Background(), q, &out, "lexify", since, testNow); err != nil {
		t.Fatal(err)
	}
	md := out.String()
	for _, s := range Sections {
		if !strings.Contains(md, "## "+s.Title) {
			t.Errorf("missing section %q", s.Title)
		}
	}
	if !strings.Contains(md, "Okno: 2026-11-07 12:00 až 2026-11-14 12:00") {
		t.Errorf("window line missing:\n%s", md[:200])
	}
	if !strings.Contains(md, "| deck | 3 |") || !strings.Contains(md, `| a\|b | 1.2 |`) {
		t.Errorf("table rows not rendered:\n%s", md)
	}
}

func TestMarkdownEmpty(t *testing.T) {
	var b bytes.Buffer
	Markdown(&b, []string{"a"}, nil)
	if b.String() != "_žádná data_\n" {
		t.Fatalf("%q", b.String())
	}
}

func TestFormat(t *testing.T) {
	var num pgtype.Numeric
	if err := num.Scan("12.50"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{true, "ano"},
		{false, "ne"},
		{int64(42), "42"},
		{int32(7), "7"},
		{3.0, "3"},
		{2.345, "2.3"},
		{num, "12.5"},
		{pgtype.Numeric{}, ""},
		{big.NewInt(9), "9"},
		{time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), "2026-11-14"},
	}
	for _, tc := range tests {
		if got := Format(tc.in); got != tc.want {
			t.Errorf("Format(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		err  bool
	}{
		{"7d", testNow.AddDate(0, 0, -7), false},
		{"30d", testNow.AddDate(0, 0, -30), false},
		{"36h", testNow.Add(-36 * time.Hour), false},
		{"2026-11-01", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), false},
		{"0d", time.Time{}, true},
		{"-5h", time.Time{}, true},
		{"week", time.Time{}, true},
	}
	for _, tc := range tests {
		got, err := ParseSince(tc.in, testNow)
		if (err != nil) != tc.err || (!tc.err && !got.Equal(tc.want)) {
			t.Errorf("ParseSince(%q) = %v, %v", tc.in, got, err)
		}
	}
}
