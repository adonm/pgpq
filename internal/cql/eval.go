package cql

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

type truth int

const (
	triUnknown truth = iota
	triFalse
	triTrue
)

func Match(e Expr, props map[string]any, columns map[string]Type) bool {
	return eval(e, props, columns) == triTrue
}

func eval(e Expr, props map[string]any, columns map[string]Type) truth {
	switch x := e.(type) {
	case *Compare:
		return evalCompare(x, props, columns)
	case *Between:
		return evalBetween(x, props, columns)
	case *In:
		return evalIn(x, props, columns)
	case *Like:
		return evalLike(x, props, columns)
	case *IsNull:
		return evalIsNull(x, props)
	case andExpr:
		return triAnd(eval(x.left, props, columns), eval(x.right, props, columns))
	case orExpr:
		return triOr(eval(x.left, props, columns), eval(x.right, props, columns))
	case notExpr:
		return triNot(eval(x.inner, props, columns))
	}
	return triUnknown
}

func evalCompare(c *Compare, props map[string]any, columns map[string]Type) truth {
	value := columnValue(c.Value, columns[c.Prop])
	if c.Op == "=" {
		if matchEquality(c.Prop, value, props) {
			return triTrue
		}
		return triFalse
	}
	return matchOrdered(c.Prop, c.Op, value, props)
}

func evalBetween(e *Between, props map[string]any, columns map[string]Type) truth {
	lo := columnValue(e.Lo, columns[e.Prop])
	hi := columnValue(e.Hi, columns[e.Prop])
	r := triAnd(matchOrdered(e.Prop, ">=", lo, props), matchOrdered(e.Prop, "<=", hi, props))
	if e.Not {
		return triNot(r)
	}
	return r
}

func evalIn(e *In, props map[string]any, columns map[string]Type) truth {
	r := triFalse
	for _, v := range e.Values {
		if matchEquality(e.Prop, columnValue(v, columns[e.Prop]), props) {
			r = triTrue
			break
		}
	}
	if e.Not {
		return triNot(r)
	}
	return r
}

func evalLike(e *Like, props map[string]any, _ map[string]Type) truth {
	value, ok := props[e.Prop]
	if !ok || value == nil {
		return triUnknown
	}
	text, ok := value.(string)
	if !ok {
		return triUnknown
	}
	r := triFalse
	if likeMatch(text, e.Pattern) {
		r = triTrue
	}
	if e.Not {
		return triNot(r)
	}
	return r
}

func evalIsNull(e *IsNull, props map[string]any) truth {
	value, ok := props[e.Prop]
	isNull := !ok || value == nil
	if e.Not {
		isNull = !isNull
	}
	if isNull {
		return triTrue
	}
	return triFalse
}

func columnValue(value any, column Type) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	if numericColumnType(column) {
		if n, ok := Number(text); ok {
			return n
		}
	}
	if column == Bool {
		switch text {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return value
}

func numericColumnType(column Type) bool {
	return column == Int || column == Double
}

func matchEquality(prop string, value any, props map[string]any) bool {
	cur, ok := props[prop]
	if !ok || cur == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return matchBool(cur, v)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return matchNumber(cur, value)
	case string:
		return matchString(cur, v)
	}
	return matchString(cur, fmt.Sprint(value))
}

func matchBool(cur any, value bool) bool {
	switch x := cur.(type) {
	case []any:
		for _, member := range x {
			if matchBool(member, value) {
				return true
			}
		}
		return false
	case bool:
		return x == value
	case string:
		return x == fmt.Sprint(value)
	}
	return false
}

func matchNumber(cur any, value any) bool {
	switch x := cur.(type) {
	case []any:
		for _, member := range x {
			if matchNumber(member, value) {
				return true
			}
		}
		return false
	case string:
		return x == fmt.Sprint(value)
	}
	if !isNumber(cur) {
		return false
	}
	cmp, ok := compareNumbers(cur, value)
	return ok && cmp == 0
}

func matchString(cur any, value string) bool {
	switch x := cur.(type) {
	case []any:
		for _, member := range x {
			if matchString(member, value) {
				return true
			}
		}
		return false
	case string:
		return x == value
	}
	return false
}

func matchOrdered(prop, op string, value any, props map[string]any) truth {
	cur, ok := props[prop]
	if !ok || cur == nil {
		return triUnknown
	}
	if bv, ok := value.(bool); ok {
		b, ok := cur.(bool)
		if !ok {
			return triUnknown
		}
		cmp := 0
		switch {
		case b == bv:
		case !b:
			cmp = -1
		default:
			cmp = 1
		}
		return fromCompare(op, cmp)
	}
	if isNumber(value) {
		if !isNumber(cur) {
			return triUnknown
		}
		cmp, ok := compareNumbers(cur, value)
		if !ok {
			return triUnknown
		}
		return fromCompare(op, cmp)
	}
	if s, ok := value.(string); ok {
		text, ok := cur.(string)
		if !ok {
			return triUnknown
		}
		return fromCompare(op, strings.Compare(text, s))
	}
	return triUnknown
}

func fromCompare(op string, cmp int) truth {
	match := false
	switch op {
	case "<":
		match = cmp < 0
	case "<=":
		match = cmp <= 0
	case ">":
		match = cmp > 0
	case ">=":
		match = cmp >= 0
	case "<>":
		match = cmp != 0
	case "=":
		match = cmp == 0
	}
	if match {
		return triTrue
	}
	return triFalse
}

func triAnd(a, b truth) truth {
	if a == triFalse || b == triFalse {
		return triFalse
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triTrue
}

func triOr(a, b truth) truth {
	if a == triTrue || b == triTrue {
		return triTrue
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triFalse
}

func triNot(a truth) truth {
	switch a {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triUnknown
}

func isNumber(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	}
	return false
}

func compareNumbers(a, b any) (int, bool) {
	ar, aok := rational(a)
	br, bok := rational(b)
	if aok && bok {
		return ar.Cmp(br), true
	}
	af, aok := floatValue(a)
	bf, bok := floatValue(b)
	if !aok || !bok || math.IsNaN(af) || math.IsNaN(bf) {
		return 0, false
	}
	switch {
	case af < bf:
		return -1, true
	case af > bf:
		return 1, true
	}
	return 0, true
}

func rational(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(x)), true
	case int8:
		return new(big.Rat).SetInt64(int64(x)), true
	case int16:
		return new(big.Rat).SetInt64(int64(x)), true
	case int32:
		return new(big.Rat).SetInt64(int64(x)), true
	case int64:
		return new(big.Rat).SetInt64(x), true
	case uint:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(x))), true
	case uint8:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(x))), true
	case uint16:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(x))), true
	case uint32:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(x))), true
	case uint64:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(x)), true
	case float32:
		return rationalFloat(float64(x))
	case float64:
		return rationalFloat(x)
	}
	return nil, false
}

func rationalFloat(f float64) (*big.Rat, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	return new(big.Rat).SetFloat64(f), true
}

func floatValue(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func likeMatch(s, pattern string) bool {
	si, pi := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pattern) {
			switch c := pattern[pi]; {
			case c == '\\' && pi+1 < len(pattern) && pattern[pi+1] == s[si]:
				pi += 2
				si++
				continue
			case c == '_':
				pi++
				si++
				continue
			case c == '%':
				star, mark = pi, si
				pi++
				continue
			case c == s[si]:
				pi++
				si++
				continue
			}
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	return pi == len(pattern)
}
