package cql

import (
	"math"
	"testing"
	"time"
)

func evalText(t *testing.T, text string, casts Casts, props map[string]any) bool {
	t.Helper()
	expr, err := Parse(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return Match(expr, props, casts)
}

func TestEvalEquality(t *testing.T) {
	numeric := Casts{"id": CastNumeric}
	cases := []struct {
		text  string
		casts Casts
		props map[string]any
		want  bool
	}{
		{"id = 5", numeric, map[string]any{"id": int64(5)}, true},
		{"id = 5", numeric, map[string]any{"id": int64(6)}, false},
		{"id = '5'", numeric, map[string]any{"id": int64(5)}, true},
		{"id = '5'", numeric, map[string]any{"id": "5"}, true},
		{"name = 5", Casts{"name": CastText}, map[string]any{"name": "5"}, true},
		{"name = 'cat_07'", Casts{"name": CastText}, map[string]any{"name": "cat_07"}, true},
		{"name = 'cat_07'", Casts{"name": CastText}, map[string]any{"name": "cat_08"}, false},
		{"tags = 'r3'", nil, map[string]any{"tags": []any{"u1", "r3"}}, true},
		{"tags = 'r3'", nil, map[string]any{"tags": []any{"u1", "u2"}}, false},
		{"id = 5", numeric, map[string]any{}, false},
		{"id = 5", numeric, map[string]any{"id": nil}, false},
	}
	for _, tc := range cases {
		if got := evalText(t, tc.text, tc.casts, tc.props); got != tc.want {
			t.Errorf("%s over %v = %v, want %v", tc.text, tc.props, got, tc.want)
		}
	}
}

func TestEvalBooleans(t *testing.T) {
	casts := Casts{"flag": CastBoolean}
	for _, tc := range []struct {
		text  string
		props map[string]any
		want  bool
	}{
		{"flag = true", map[string]any{"flag": true}, true},
		{"flag = 'true'", map[string]any{"flag": true}, true},
		{"flag = true", map[string]any{"flag": false}, false},
		{"flag <> true", map[string]any{"flag": false}, true},
		{"flag <> true", map[string]any{"flag": true}, false},
		{"flag <> true", map[string]any{}, false},
	} {
		if got := evalText(t, tc.text, casts, tc.props); got != tc.want {
			t.Errorf("%s over %v = %v, want %v", tc.text, tc.props, got, tc.want)
		}
	}
}

func TestEvalOrdered(t *testing.T) {
	numeric := Casts{"n": CastNumeric}
	for _, tc := range []struct {
		text  string
		casts Casts
		props map[string]any
		want  bool
	}{
		{"n > 3", numeric, map[string]any{"n": int64(4)}, true},
		{"n > 3", numeric, map[string]any{"n": int64(3)}, false},
		{"n >= 3", numeric, map[string]any{"n": int64(3)}, true},
		{"n < 3", numeric, map[string]any{"n": float64(2.5)}, true},
		{"n <= 3", numeric, map[string]any{"n": float64(3)}, true},
		{"n > 3", numeric, map[string]any{"n": "4"}, false},
		{"n > 3", numeric, map[string]any{}, false},
		{"n > 3", numeric, map[string]any{"n": nil}, false},
		{"n > 3", numeric, map[string]any{"n": math.NaN()}, false},
		{"s > 'B'", Casts{"s": CastText}, map[string]any{"s": "a"}, true},
		{"s > 'B'", Casts{"s": CastText}, map[string]any{"s": "A"}, false},
		{"s <> 'cat_00'", Casts{"s": CastText}, map[string]any{"s": "cat_01"}, true},
		{"s <> 'cat_00'", Casts{"s": CastText}, map[string]any{"s": "cat_00"}, false},
		{"s <> 'cat_00'", Casts{"s": CastText}, map[string]any{}, false},
		{"s > 'a'", Casts{"s": CastText}, map[string]any{"s": []any{"b", "a"}}, false},
	} {
		if got := evalText(t, tc.text, tc.casts, tc.props); got != tc.want {
			t.Errorf("%s over %v = %v, want %v", tc.text, tc.props, got, tc.want)
		}
	}
}

func TestEvalBetweenAndIn(t *testing.T) {
	numeric := Casts{"n": CastNumeric}
	for _, tc := range []struct {
		text  string
		casts Casts
		props map[string]any
		want  bool
	}{
		{"n BETWEEN 1 AND 5", numeric, map[string]any{"n": int64(3)}, true},
		{"n BETWEEN 1 AND 5", numeric, map[string]any{"n": int64(6)}, false},
		{"n BETWEEN 1 AND 5", numeric, map[string]any{}, false},
		{"n NOT BETWEEN 1 AND 5", numeric, map[string]any{"n": int64(6)}, true},
		{"n IN (1, 2, 3)", numeric, map[string]any{"n": int64(2)}, true},
		{"n IN ('1', '2')", numeric, map[string]any{"n": int64(1)}, true},
		{"n IN (1, 2, 3)", numeric, map[string]any{"n": int64(4)}, false},
		{"s IN ('a', 'b')", Casts{"s": CastText}, map[string]any{"s": "b"}, true},
	} {
		if got := evalText(t, tc.text, tc.casts, tc.props); got != tc.want {
			t.Errorf("%s over %v = %v, want %v", tc.text, tc.props, got, tc.want)
		}
	}
}

func TestEvalLikeAndNull(t *testing.T) {
	casts := Casts{"s": CastText}
	for _, tc := range []struct {
		text  string
		props map[string]any
		want  bool
	}{
		{"s LIKE 'cat_1%'", map[string]any{"s": "cat_12"}, true},
		{"s LIKE 'cat_1%'", map[string]any{"s": "cat_02"}, false},
		{"s LIKE 'cat_1_'", map[string]any{"s": "cat_12"}, true},
		{"s LIKE 'a\\%b'", map[string]any{"s": "a%b"}, true},
		{"s LIKE 'a\\%b'", map[string]any{"s": "axb"}, false},
		{"s LIKE 'a\\_b'", map[string]any{"s": "a_b"}, true},
		{"s LIKE 'cat%'", map[string]any{}, false},
		{"s LIKE 'cat%'", map[string]any{"s": nil}, false},
		{"s IS NULL", map[string]any{}, true},
		{"s IS NULL", map[string]any{"s": nil}, true},
		{"s IS NULL", map[string]any{"s": "x"}, false},
		{"s IS NOT NULL", map[string]any{"s": "x"}, true},
		{"s IS NOT NULL", map[string]any{}, false},
		{"unknown IS NULL", map[string]any{"s": "x"}, true},
	} {
		if got := evalText(t, tc.text, casts, tc.props); got != tc.want {
			t.Errorf("%s over %v = %v, want %v", tc.text, tc.props, got, tc.want)
		}
	}
}

func TestEvalLogic(t *testing.T) {
	numeric := Casts{"n": CastNumeric}
	for _, tc := range []struct {
		text  string
		props map[string]any
		want  bool
	}{
		{"n = 1 AND n > 0", map[string]any{"n": int64(1)}, true},
		{"n = 1 AND n > 0", map[string]any{}, false},
		{"n = 1 OR n = 2", map[string]any{"n": int64(2)}, true},
		{"NOT (n > 5)", map[string]any{"n": int64(1)}, true},
		{"NOT (n > 5)", map[string]any{}, false},
		{"NOT (n = 5)", map[string]any{}, true},
		{"NOT (n = 5)", map[string]any{"n": int64(5)}, false},
		{"NOT (n = 5 OR n = 6)", map[string]any{"n": int64(7)}, true},
	} {
		if got := evalText(t, tc.text, numeric, tc.props); got != tc.want {
			t.Errorf("%s over %v = %v, want %v", tc.text, tc.props, got, tc.want)
		}
	}
}

func TestEvalDatetime(t *testing.T) {
	casts := Casts{"c_date": CastNone}
	lo := time.Date(2026, 1, 5, 0, 30, 0, 0, time.UTC)
	hi := time.Date(2026, 1, 6, 0, 30, 0, 0, time.UTC)
	window := And(&Compare{Prop: "c_date", Op: ">=", Value: lo}, &Compare{Prop: "c_date", Op: "<=", Value: hi})
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"2026-01-05T12:00:00Z", true},
		{"2026-01-06T01:00:00Z", false},
		{"2026-01-05T12:00:00", true},
		{"garbage", false},
	} {
		if got := Match(window, map[string]any{"c_date": tc.value}, casts); got != tc.want {
			t.Errorf("datetime window over %q = %v, want %v", tc.value, got, tc.want)
		}
	}
	if Match(&Compare{Prop: "c_date", Op: ">", Value: lo}, map[string]any{"c_date": "2026-01-05T00:30:00Z"}, casts) {
		t.Error("equal timestamps are not strictly greater")
	}
	if !Match(&Compare{Prop: "c_date", Op: ">=", Value: lo}, map[string]any{"c_date": "2026-01-05T00:30:00Z"}, casts) {
		t.Error("a zone-less stored value should parse as UTC and satisfy >=")
	}
}

func TestEvalNumericEqualityAcrossForms(t *testing.T) {
	if !evalText(t, "n = 2", Casts{"n": CastNumeric}, map[string]any{"n": float64(2)}) {
		t.Error("2 should equal 2.0")
	}
	if !evalText(t, "n = 2.0", Casts{"n": CastNumeric}, map[string]any{"n": int64(2)}) {
		t.Error("2.0 should equal 2")
	}
	if !evalText(t, "n = 5", Casts{"n": CastText}, map[string]any{"n": "5"}) {
		t.Error("numeric literal should match its string form")
	}
}
