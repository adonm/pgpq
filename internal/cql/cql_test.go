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
	sql, args := sqlOf(expr, nil)
	want := "((case when jsonb_typeof(props -> $1) = 'number' then (props ->> $1)::numeric end) >= $2" +
		" and (props @> jsonb_build_object($3::text, $4::text) or props @> jsonb_build_object($3::text, jsonb_build_array($4::text))))"
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
	sql, args := sqlOf(expr, nil)
	want := "((props @> jsonb_build_object($1::text, $2::text) or props @> jsonb_build_object($1::text, jsonb_build_array($2::text))) and (((props @> jsonb_build_object($3::text, $4::numeric) or props @> jsonb_build_object($3::text, jsonb_build_array($4::numeric))) or (props @> jsonb_build_object($3::text, $5::text) or props @> jsonb_build_object($3::text, jsonb_build_array($5::text)))) or ((props @> jsonb_build_object($6::text, $7::numeric) or props @> jsonb_build_object($6::text, jsonb_build_array($7::numeric))) or (props @> jsonb_build_object($6::text, $8::text) or props @> jsonb_build_object($6::text, jsonb_build_array($8::text))))))"
	if sql != want {
		t.Fatalf("sql = %s, want %s", sql, want)
	}
	if len(args) != 8 {
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
		textSQL, textArgs := sqlOf(textExpr, nil)
		jsonSQL, jsonArgs := sqlOf(jsonParsed, nil)
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
		{`i in (1, 2)`, 6, " or "},
		{`i not between 1 and 5`, 3, "between $2 and $3"},
		{`x is null`, 1, "is null or jsonb_typeof"},
		{`x is not null`, 1, "not ("},
		{`not (a = 'b' or c > 1)`, 4, "not ("},
		{`flag = true`, 3, "jsonb_build_object"},
		{`i <> 3`, 2, "<>"},
		{`i != 3`, 2, "<>"},
	}
	for _, tc := range cases {
		expr, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		var args []any
		sql, args := sqlOf(expr, nil)
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

func TestSpatialFunctionsAreRejected(t *testing.T) {
	_, err := Parse(`name = 'x' and S_INTERSECTS(geometry, POINT(1 2))`)
	if err == nil || !strings.Contains(err.Error(), "spatial functions are not supported") {
		t.Fatalf("text spatial function: %v", err)
	}
	if _, err := ParseJSON([]byte(`{"op":"s_intersects","args":[{"property":"geometry"},{"type":"Point","coordinates":[1,2]}]}`)); err == nil {
		t.Fatal("json spatial operator should be rejected")
	}
}

func TestTypedColumnsUseTheIndexedExpression(t *testing.T) {
	text, err := Parse("diets_vegan >= 'yes'")
	if err != nil {
		t.Fatal(err)
	}
	sql, args := sqlOf(text, Casts{"diets_vegan": CastText})
	if want := ColumnExpr("props", "diets_vegan", CastText) + " >= $1"; sql != want {
		t.Fatalf("text column = %s, want %s", sql, want)
	}
	if value, ok := args[0].(string); !ok || value != "yes" {
		t.Fatalf("bound value = %#v", args[0])
	}

	number, err := Parse("population > '10'")
	if err != nil {
		t.Fatal(err)
	}
	sql, numberArgs := sqlOf(number, Casts{"population": CastNumeric})
	if want := ColumnExpr("props", "population", CastNumeric) + " > $1"; sql != want {
		t.Fatalf("numeric column = %s, want %s", sql, want)
	}
	if value, ok := numberArgs[0].(int64); !ok || value != 10 {
		t.Fatalf("bound value = %#v", numberArgs[0])
	}

	unknown, err := Parse("population > '10'")
	if err != nil {
		t.Fatal(err)
	}
	if sql, unknownArgs := sqlOf(unknown, nil); !strings.Contains(sql, "jsonb_typeof(props -> $1)") {
		t.Fatalf("unknown property should keep its key as a parameter: %s", sql)
	} else if len(unknownArgs) != 2 {
		t.Fatalf("unknown property bound %d args", len(unknownArgs))
	}
}

func sqlOf(e Expr, casts Casts) (string, []any) {
	b := NewBuilder("props", casts)
	return e.SQL(b), b.Args()
}
