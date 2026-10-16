package cql

import (
	"reflect"
	"strings"
	"testing"
)

func TestSQL(t *testing.T) {
	expr, err := Parse(`i >= 42 and name = 'x''y'`)
	if err != nil {
		t.Fatal(err)
	}
	var args []any
	sql := expr.SQL("props", &args)
	want := "((case when jsonb_typeof(props -> $1) = 'number' then (props ->> $1)::numeric end) >= $2" +
		" and props @> jsonb_build_object($3::text, $4::text))"
	if sql != want {
		t.Fatalf("sql = %s, want %s", sql, want)
	}
	wantArgs := []any{"i", int64(42), "name", "x'y"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
}

func TestEqualityUsesContainment(t *testing.T) {
	expr, err := Parse(`name = 'x' and i in (1,2)`)
	if err != nil {
		t.Fatal(err)
	}
	var args []any
	sql := expr.SQL("props", &args)
	want := "(props @> jsonb_build_object($1::text, $2::text) and (props @> jsonb_build_object($3::text, $4::numeric) or props @> jsonb_build_object($5::text, $6::numeric)))"
	if sql != want {
		t.Fatalf("sql = %s, want %s", sql, want)
	}
	if len(args) != 6 {
		t.Fatalf("args = %v", args)
	}
}

func TestClauses(t *testing.T) {
	cases := []struct {
		in       string
		argCount int
		contains string
	}{
		{`name like 'a%'`, 2, "like $2"},
		{`i in (1, 2)`, 4, " or "},
		{`i not between 1 and 5`, 3, "between $2 and $3"},
		{`x is null`, 1, "is null or jsonb_typeof"},
		{`x is not null`, 1, "not ("},
		{`not (a = 'b' or c > 1)`, 4, "not ("},
		{`flag = true`, 2, "jsonb_build_object"},
		{`i <> 3`, 2, "<>"},
		{`i != 3`, 2, "<>"},
	}
	for _, tc := range cases {
		expr, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		var args []any
		sql := expr.SQL("props", &args)
		if len(args) != tc.argCount {
			t.Fatalf("%s: %d args, want %d", tc.in, len(args), tc.argCount)
		}
		if !strings.Contains(sql, tc.contains) {
			t.Fatalf("%s: sql %s missing %q", tc.in, sql, tc.contains)
		}
	}
}

func TestErrors(t *testing.T) {
	bad := []string{
		`i =`,
		`name like 5`,
		`i between 1 and 'x'`,
		`i = 1 junk`,
		`name = 'x`,
		`in (1)`,
		`i is 5`,
		`(i = 1`,
	}
	for _, in := range bad {
		if _, err := Parse(in); err == nil {
			t.Fatalf("%s: expected error", in)
		}
	}
}
