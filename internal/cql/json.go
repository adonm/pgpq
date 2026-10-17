package cql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/adonm/postfgb/internal/geo"
)

func ParseJSON(body []byte) (Expr, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var node any
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("cql2-json: %w", err)
	}
	return jsonExpr(node)
}

func jsonExpr(v any) (Expr, error) {
	node, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("cql2-json: expected an expression object")
	}
	op, _ := node["op"].(string)
	if op == "" {
		return nil, errors.New("cql2-json: missing op")
	}
	args, _ := node["args"].([]any)
	switch strings.ToLower(op) {
	case "and", "or":
		if len(args) < 2 {
			return nil, fmt.Errorf("cql2-json: %s needs at least two arguments", op)
		}
		expr, err := jsonExpr(args[0])
		if err != nil {
			return nil, err
		}
		for _, arg := range args[1:] {
			next, err := jsonExpr(arg)
			if err != nil {
				return nil, err
			}
			if strings.EqualFold(op, "and") {
				expr = andExpr{expr, next}
			} else {
				expr = orExpr{expr, next}
			}
		}
		return expr, nil
	case "not":
		if len(args) != 1 {
			return nil, errors.New("cql2-json: not needs exactly one argument")
		}
		inner, err := jsonExpr(args[0])
		if err != nil {
			return nil, err
		}
		return notExpr{inner}, nil
	case "=", "<>", "<", "<=", ">", ">=":
		if len(args) != 2 {
			return nil, fmt.Errorf("cql2-json: %s needs two arguments", op)
		}
		prop, value, err := jsonComparison(args)
		if err != nil {
			return nil, err
		}
		return &Compare{Prop: prop, Op: op, Value: value}, nil
	case "s_intersects", "intersects":
		if len(args) != 2 {
			return nil, errors.New("cql2-json: s_intersects needs two arguments")
		}
		if _, err := jsonProperty(args[0]); err != nil {
			return nil, err
		}
		body, err := json.Marshal(args[1])
		if err != nil {
			return nil, err
		}
		shape, err := geo.ParseGeoJSON(body)
		if err != nil {
			return nil, fmt.Errorf("cql2-json: %w", err)
		}
		return &Spatial{Fn: "s_intersects", Shape: shape}, nil
	case "!=":
		if len(args) != 2 {
			return nil, errors.New("cql2-json: != needs two arguments")
		}
		prop, value, err := jsonComparison(args)
		if err != nil {
			return nil, err
		}
		return &Compare{Prop: prop, Op: "<>", Value: value}, nil
	case "between":
		if len(args) != 3 {
			return nil, errors.New("cql2-json: between needs three arguments")
		}
		prop, err := jsonProperty(args[0])
		if err != nil {
			return nil, err
		}
		lo, err := jsonLiteral(args[1])
		if err != nil {
			return nil, err
		}
		hi, err := jsonLiteral(args[2])
		if err != nil {
			return nil, err
		}
		if valueKind(lo) != valueKind(hi) {
			return nil, errors.New("cql2-json: between bounds must share a type")
		}
		return &Between{Prop: prop, Lo: lo, Hi: hi}, nil
	case "in":
		if len(args) != 2 {
			return nil, errors.New("cql2-json: in needs two arguments")
		}
		prop, err := jsonProperty(args[0])
		if err != nil {
			return nil, err
		}
		values, ok := args[1].([]any)
		if !ok {
			return nil, errors.New("cql2-json: in needs a value list")
		}
		expr := &In{Prop: prop}
		for _, v := range values {
			lit, err := jsonLiteral(v)
			if err != nil {
				return nil, err
			}
			expr.Values = append(expr.Values, lit)
		}
		return expr, nil
	case "like":
		if len(args) != 2 {
			return nil, errors.New("cql2-json: like needs two arguments")
		}
		prop, err := jsonProperty(args[0])
		if err != nil {
			return nil, err
		}
		pattern, err := jsonLiteral(args[1])
		if err != nil {
			return nil, err
		}
		text, ok := pattern.(string)
		if !ok {
			return nil, errors.New("cql2-json: like requires a string pattern")
		}
		return &Like{Prop: prop, Pattern: text}, nil
	case "isnull":
		if len(args) != 1 {
			return nil, errors.New("cql2-json: isNull needs one argument")
		}
		prop, err := jsonProperty(args[0])
		if err != nil {
			return nil, err
		}
		return &IsNull{Prop: prop}, nil
	default:
		return nil, fmt.Errorf("cql2-json: unsupported operator %q", op)
	}
}

func jsonComparison(args []any) (string, any, error) {
	if prop, err := jsonProperty(args[0]); err == nil {
		value, err := jsonLiteral(args[1])
		return prop, value, err
	}
	if prop, err := jsonProperty(args[1]); err == nil {
		value, err := jsonLiteral(args[0])
		return prop, value, err
	}
	return "", nil, errors.New("cql2-json: comparison needs a property reference")
}

func jsonProperty(v any) (string, error) {
	node, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("cql2-json: expected a property reference")
	}
	name, ok := node["property"].(string)
	if !ok {
		return "", errors.New("cql2-json: expected a property reference")
	}
	return name, nil
}

func jsonLiteral(v any) (any, error) {
	if node, ok := v.(map[string]any); ok {
		lit, ok := node["literal"]
		if !ok {
			return nil, errors.New("cql2-json: unexpected object as literal")
		}
		v = lit
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return t, nil
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		if f, err := t.Float64(); err == nil {
			return f, nil
		}
		return nil, fmt.Errorf("cql2-json: invalid number %q", t)
	default:
		return nil, errors.New("cql2-json: unsupported literal value")
	}
}
