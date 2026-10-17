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
		" and (props @> jsonb_build_object($3::text, $4::text) or (jsonb_typeof(props -> $3::text) = 'array' and props -> $3::text @> jsonb_build_array($4::text))))"
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
	want := "((props @> jsonb_build_object($1::text, $2::text) or (jsonb_typeof(props -> $1::text) = 'array' and props -> $1::text @> jsonb_build_array($2::text))) and ((props @> jsonb_build_object($3::text, $4::numeric) or (jsonb_typeof(props -> $3::text) = 'array' and props -> $3::text @> jsonb_build_array($4::numeric))) or (props @> jsonb_build_object($5::text, $6::numeric) or (jsonb_typeof(props -> $5::text) = 'array' and props -> $5::text @> jsonb_build_array($6::numeric)))))"
	if sql != want {
		t.Fatalf("sql = %s, want %s", sql, want)
	}
	if len(args) != 6 {
		t.Fatalf("args = %v", args)
	}
}

func TestJSONParity(t *testing.T) {
	cases := []struct{ text, json string }{
		{`i > 42`, `{"op":">","args":[{"property":"i"},42]}`},
		{`name = 'x'`, `{"op":"=","args":[{"property":"name"},"x"]}`},
		{`name like 'a%' and i between 1 and 5`, `{"op":"and","args":[{"op":"like","args":[{"property":"name"},"a%"]},{"op":"between","args":[{"property":"i"},1,5]}]}`},
		{`i in (1, 2, 3)`, `{"op":"in","args":[{"property":"i"},[1,2,3]]}`},
		{`x is null`, `{"op":"isNull","args":[{"property":"x"}]}`},
		{`not (a = 'b' or c > 1)`, `{"op":"not","args":[{"op":"or","args":[{"op":"=","args":[{"property":"a"},"b"]},{"op":">","args":[{"property":"c"},1]}]}]}`},
		{`i <> 3`, `{"op":"<>","args":[{"property":"i"},3]}`},
		{`i != 3`, `{"op":"!=","args":[{"property":"i"},3]}`},
	}
	for _, tc := range cases {
		textExpr, err := Parse(tc.text)
		if err != nil {
			t.Fatalf("%s: %v", tc.text, err)
		}
		jsonParsed, err := ParseJSON([]byte(tc.json))
		if err != nil {
			t.Fatalf("%s: %v", tc.json, err)
		}
		var textArgs, jsonArgs []any
		textSQL := textExpr.SQL("props", &textArgs)
		jsonSQL := jsonParsed.SQL("props", &jsonArgs)
		if textSQL != jsonSQL {
			t.Fatalf("%s: sql %q != %q", tc.text, textSQL, jsonSQL)
		}
		if !reflect.DeepEqual(textArgs, jsonArgs) {
			t.Fatalf("%s: args %v != %v", tc.text, textArgs, jsonArgs)
		}
	}
}

func TestJSONErrors(t *testing.T) {
	bad := []string{
		`{"op":"frobnicate","args":[]}`,
		`{"args":[]}`,
		`{"op":"like","args":[{"property":"x"},5]}`,
		`{"op":"=","args":["a","b"]}`,
		`{`,
	}
	for _, in := range bad {
		if _, err := ParseJSON([]byte(in)); err == nil {
			t.Fatalf("%s: expected error", in)
		}
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
