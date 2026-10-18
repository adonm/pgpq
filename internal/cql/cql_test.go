package cql

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseBuildsTheTree(t *testing.T) {
	expr, err := Parse(`i >= 42 and name = 'x''y'`)
	if err != nil {
		t.Fatal(err)
	}
	want := andExpr{
		left:  &Compare{Prop: "i", Op: ">=", Value: int64(42)},
		right: &Compare{Prop: "name", Op: "=", Value: "x'y"},
	}
	if !reflect.DeepEqual(expr, want) {
		t.Fatalf("tree = %#v, want %#v", expr, want)
	}
}

func TestParseClauses(t *testing.T) {
	cases := []struct {
		in   string
		want Expr
	}{
		{`name like 'a%'`, &Like{Prop: "name", Pattern: "a%"}},
		{`name not like 'a%'`, &Like{Prop: "name", Pattern: "a%", Not: true}},
		{`i in (1, 2)`, &In{Prop: "i", Values: []any{int64(1), int64(2)}}},
		{`i not in (1)`, &In{Prop: "i", Values: []any{int64(1)}, Not: true}},
		{`i between 1 and 5`, &Between{Prop: "i", Lo: int64(1), Hi: int64(5)}},
		{`i not between 1 and 5`, &Between{Prop: "i", Lo: int64(1), Hi: int64(5), Not: true}},
		{`x is null`, &IsNull{Prop: "x"}},
		{`x is not null`, &IsNull{Prop: "x", Not: true}},
		{`flag = true`, &Compare{Prop: "flag", Op: "=", Value: true}},
		{`i <> 3`, &Compare{Prop: "i", Op: "<>", Value: int64(3)}},
		{`i != 3`, &Compare{Prop: "i", Op: "<>", Value: int64(3)}},
		{`not (a = 'b' or c > 1)`, notExpr{orExpr{
			left:  &Compare{Prop: "a", Op: "=", Value: "b"},
			right: &Compare{Prop: "c", Op: ">", Value: int64(1)},
		}}},
	}
	for _, tc := range cases {
		expr, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if !reflect.DeepEqual(expr, tc.want) {
			t.Fatalf("%s: tree = %#v, want %#v", tc.in, expr, tc.want)
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
}
