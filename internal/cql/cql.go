package cql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Cast string

const (
	CastNone      Cast = ""
	CastText      Cast = "text"
	CastNumeric   Cast = "numeric"
	CastBoolean   Cast = "boolean"
	CastTimestamp Cast = "timestamptz"
)

type Casts map[string]Cast

type Builder struct {
	Col   string
	Casts Casts
	args  []any
}

func NewBuilder(col string, casts Casts, args ...any) *Builder {
	return &Builder{Col: col, Casts: casts, args: args}
}

func (b *Builder) Bind(value any) string {
	b.args = append(b.args, value)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *Builder) Args() []any {
	return b.args
}

type Expr interface {
	SQL(b *Builder) string
}

func ColumnExpr(col, prop string, cast Cast) string {
	key := strings.ReplaceAll(prop, "'", "''")
	lit := col + " -> '" + key + "'"
	text := col + " ->> '" + key + "'"
	switch cast {
	case CastNumeric:
		return "(case when jsonb_typeof(" + lit + ") = 'number' then (" + text + ")::numeric end)"
	case CastBoolean:
		return "(case when jsonb_typeof(" + lit + ") = 'boolean' then (" + text + ")::boolean end)"
	case CastTimestamp:
		return "(case when jsonb_typeof(" + lit + ") = 'string' then (" + text + ")::timestamptz end)"
	default:
		return "(case when jsonb_typeof(" + lit + ") = 'string' then " + text + " end)"
	}
}

func And(left, right Expr) Expr {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return andExpr{left, right}
}

type andExpr struct{ left, right Expr }

func (e andExpr) SQL(b *Builder) string {
	return "(" + e.left.SQL(b) + " and " + e.right.SQL(b) + ")"
}

type orExpr struct{ left, right Expr }

func (e orExpr) SQL(b *Builder) string {
	return "(" + e.left.SQL(b) + " or " + e.right.SQL(b) + ")"
}

type notExpr struct{ inner Expr }

func (e notExpr) SQL(b *Builder) string {
	return "(not " + e.inner.SQL(b) + ")"
}

type Compare struct {
	Prop  string
	Op    string
	Value any
}

func (e *Compare) SQL(b *Builder) string {
	if e.Op == "=" {
		return equalityExpr(b, e.Prop, e.Value)
	}
	value := coerce(e.Value, b.Casts[e.Prop])
	return valueExpr(b, e.Prop, value) + " " + e.Op + " " + b.Bind(value)
}

type Between struct {
	Prop   string
	Lo, Hi any
	Not    bool
}

func (e *Between) SQL(b *Builder) string {
	lo := coerce(e.Lo, b.Casts[e.Prop])
	hi := coerce(e.Hi, b.Casts[e.Prop])
	sql := valueExpr(b, e.Prop, lo) +
		" between " + b.Bind(lo) + " and " + b.Bind(hi)
	if e.Not {
		return "(not " + sql + ")"
	}
	return sql
}

type In struct {
	Prop   string
	Values []any
	Not    bool
}

func (e *In) SQL(b *Builder) string {
	parts := make([]string, len(e.Values))
	for i, v := range e.Values {
		parts[i] = equalityExpr(b, e.Prop, v)
	}
	sql := "(" + strings.Join(parts, " or ") + ")"
	if e.Not {
		return "(not " + sql + ")"
	}
	return sql
}

type Like struct {
	Prop    string
	Pattern string
	Not     bool
}

func (e *Like) SQL(b *Builder) string {
	sql := valueExpr(b, e.Prop, "") + " like " + b.Bind(e.Pattern)
	if e.Not {
		return "(not " + sql + ")"
	}
	return sql
}

type IsNull struct {
	Prop string
	Not  bool
}

func (e *IsNull) SQL(b *Builder) string {
	key := b.Bind(e.Prop)
	cond := fmt.Sprintf("(%s -> %s is null or jsonb_typeof(%s -> %s) = 'null')", b.Col, key, b.Col, key)
	if e.Not {
		return "(not " + cond + ")"
	}
	return cond
}

func equalityExpr(b *Builder, prop string, value any) string {
	key := b.Bind(prop) + "::text"
	clauses := make([]string, 0, 2)
	switch value.(type) {
	case bool:
		clauses = append(clauses, equalityClause(b.Col, key, b.Bind(value)+"::boolean"))
		clauses = append(clauses, equalityClause(b.Col, key, b.Bind(value)+"::text"))
	case int, int64, float64:
		clauses = append(clauses, equalityClause(b.Col, key, b.Bind(value)+"::numeric"))
		clauses = append(clauses, equalityClause(b.Col, key, b.Bind(fmt.Sprint(value))+"::text"))
	default:
		clauses = append(clauses, equalityClause(b.Col, key, b.Bind(value)+"::text"))
	}
	if len(clauses) == 1 {
		return clauses[0]
	}
	return "(" + strings.Join(clauses, " or ") + ")"
}

func equalityClause(col, key, val string) string {
	return fmt.Sprintf("(%s @> jsonb_build_object(%s, %s) or %s @> jsonb_build_object(%s, jsonb_build_array(%s)))",
		col, key, val, col, key, val)
}

func valueExpr(b *Builder, prop string, value any) string {
	if cast, ok := b.Casts[prop]; ok && indexable(value, cast) {
		return ColumnExpr(b.Col, prop, cast)
	}
	key := b.Bind(prop)
	switch value.(type) {
	case bool:
		return fmt.Sprintf("(case when jsonb_typeof(%s -> %s) = 'boolean' then (%s ->> %s)::boolean end)", b.Col, key, b.Col, key)
	case int, int64, float64:
		return fmt.Sprintf("(case when jsonb_typeof(%s -> %s) = 'number' then (%s ->> %s)::numeric end)", b.Col, key, b.Col, key)
	case time.Time:
		return fmt.Sprintf("(case when jsonb_typeof(%s -> %s) = 'string' then (%s ->> %s)::timestamptz end)", b.Col, key, b.Col, key)
	default:
		return fmt.Sprintf("(case when jsonb_typeof(%s -> %s) = 'string' then %s ->> %s end)", b.Col, key, b.Col, key)
	}
}

func indexable(value any, cast Cast) bool {
	switch cast {
	case CastNumeric:
		switch value.(type) {
		case int, int64, float64:
			return true
		}
	case CastBoolean:
		_, ok := value.(bool)
		return ok
	case CastTimestamp:
		_, ok := value.(time.Time)
		return ok
	case CastText, CastNone:
		_, ok := value.(string)
		return ok
	}
	return false
}

func coerce(value any, cast Cast) any {
	switch cast {
	case CastNumeric:
		switch v := value.(type) {
		case int, int64, float64:
			return v
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	case CastBoolean:
		if v, ok := value.(string); ok {
			switch v {
			case "true":
				return true
			case "false":
				return false
			}
		}
	}
	return value
}

var keywords = map[string]bool{
	"and": true, "or": true, "not": true, "in": true, "between": true,
	"like": true, "is": true, "null": true, "true": true, "false": true,
}

type token struct {
	kind  string
	text  string
	value any
}

func Parse(input string) (Expr, error) {
	tokens, err := lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.tokens) {
		return nil, fmt.Errorf("cql: unexpected token %q", p.tokens[p.pos].text)
	}
	return expr, nil
}

func lex(input string) ([]token, error) {
	var tokens []token
	runes := []rune(input)
	i := 0
	for i < len(runes) {
		r := runes[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			tokens = append(tokens, token{kind: "lparen", text: "("})
			i++
		case r == ')':
			tokens = append(tokens, token{kind: "rparen", text: ")"})
			i++
		case r == ',':
			tokens = append(tokens, token{kind: "comma", text: ","})
			i++
		case r == '\'':
			i++
			var b strings.Builder
			closed := false
			for i < len(runes) {
				if runes[i] == '\'' {
					if i+1 < len(runes) && runes[i+1] == '\'' {
						b.WriteRune('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteRune(runes[i])
				i++
			}
			if !closed {
				return nil, errors.New("cql: unterminated string")
			}
			tokens = append(tokens, token{kind: "string", text: b.String(), value: b.String()})
		case unicode.IsDigit(r) || (r == '-' && i+1 < len(runes) && unicode.IsDigit(runes[i+1])):
			start := i
			if r == '-' {
				i++
			}
			for i < len(runes) && (unicode.IsDigit(runes[i]) || strings.ContainsRune(".eE+-", runes[i])) {
				i++
			}
			text := string(runes[start:i])
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				tokens = append(tokens, token{kind: "number", text: text, value: n})
			} else if f, err := strconv.ParseFloat(text, 64); err == nil {
				tokens = append(tokens, token{kind: "number", text: text, value: f})
			} else {
				return nil, fmt.Errorf("cql: invalid number %q", text)
			}
		case unicode.IsLetter(r) || r == '_':
			start := i
			for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_') {
				i++
			}
			text := string(runes[start:i])
			lower := strings.ToLower(text)
			switch {
			case lower == "true" || lower == "false":
				tokens = append(tokens, token{kind: "bool", text: text, value: lower == "true"})
			case keywords[lower]:
				tokens = append(tokens, token{kind: "keyword", text: lower})
			default:
				tokens = append(tokens, token{kind: "ident", text: text})
			}
		case r == '=':
			tokens = append(tokens, token{kind: "op", text: "="})
			i++
		case r == '<':
			switch {
			case i+1 < len(runes) && runes[i+1] == '=':
				tokens = append(tokens, token{kind: "op", text: "<="})
				i += 2
			case i+1 < len(runes) && runes[i+1] == '>':
				tokens = append(tokens, token{kind: "op", text: "<>"})
				i += 2
			default:
				tokens = append(tokens, token{kind: "op", text: "<"})
				i++
			}
		case r == '>':
			if i+1 < len(runes) && runes[i+1] == '=' {
				tokens = append(tokens, token{kind: "op", text: ">="})
				i += 2
			} else {
				tokens = append(tokens, token{kind: "op", text: ">"})
				i++
			}
		case r == '!':
			if i+1 < len(runes) && runes[i+1] == '=' {
				tokens = append(tokens, token{kind: "op", text: "<>"})
				i += 2
			} else {
				return nil, errors.New("cql: unexpected character \"!\"")
			}
		default:
			return nil, fmt.Errorf("cql: unexpected character %q", string(r))
		}
	}
	return tokens, nil
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *parser) next() *token {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}

func (p *parser) matchKeyword(word string) bool {
	t := p.peek()
	if t != nil && t.kind == "keyword" && t.text == word {
		p.pos++
		return true
	}
	return false
}

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.matchKeyword("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orExpr{left, right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.matchKeyword("and") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = andExpr{left, right}
	}
	return left, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.matchKeyword("not") {
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return notExpr{inner}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	if t := p.peek(); t != nil && t.kind == "lparen" {
		p.pos++
		expr, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		t := p.next()
		if t == nil || t.kind != "rparen" {
			return nil, errors.New("cql: missing closing parenthesis")
		}
		return expr, nil
	}
	return p.parsePredicate()
}

func (p *parser) parsePredicate() (Expr, error) {
	if t := p.peek(); t != nil && t.kind == "ident" && spatialFunction(t.text) {
		if next := p.peekAt(1); next != nil && next.kind == "lparen" {
			return p.parseSpatial()
		}
	}
	t := p.next()
	if t == nil {
		return nil, errors.New("cql: unexpected end of input")
	}
	if t.kind != "ident" {
		return nil, fmt.Errorf("cql: expected property, got %q", t.text)
	}
	prop := t.text
	t = p.next()
	if t == nil {
		return nil, errors.New("cql: unexpected end of input")
	}
	switch {
	case t.kind == "op":
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &Compare{Prop: prop, Op: t.text, Value: value}, nil
	case t.kind == "keyword" && t.text == "like":
		return p.parseLike(prop, false)
	case t.kind == "keyword" && t.text == "in":
		return p.parseIn(prop, false)
	case t.kind == "keyword" && t.text == "between":
		return p.parseBetween(prop, false)
	case t.kind == "keyword" && t.text == "is":
		not := p.matchKeyword("not")
		if !p.matchKeyword("null") {
			return nil, errors.New("cql: expected null after is")
		}
		return &IsNull{Prop: prop, Not: not}, nil
	case t.kind == "keyword" && t.text == "not":
		next := p.next()
		if next == nil {
			return nil, errors.New("cql: unexpected end of input")
		}
		switch {
		case next.kind == "keyword" && next.text == "like":
			return p.parseLike(prop, true)
		case next.kind == "keyword" && next.text == "in":
			return p.parseIn(prop, true)
		case next.kind == "keyword" && next.text == "between":
			return p.parseBetween(prop, true)
		}
		return nil, fmt.Errorf("cql: unexpected token %q after not", next.text)
	default:
		return nil, fmt.Errorf("cql: unexpected token %q", t.text)
	}
}

func (p *parser) parseLike(prop string, not bool) (Expr, error) {
	t := p.next()
	if t == nil || t.kind != "string" {
		return nil, errors.New("cql: like requires a string pattern")
	}
	return &Like{Prop: prop, Pattern: t.value.(string), Not: not}, nil
}

func (p *parser) parseIn(prop string, not bool) (Expr, error) {
	t := p.next()
	if t == nil || t.kind != "lparen" {
		return nil, errors.New("cql: in requires a value list")
	}
	expr := &In{Prop: prop, Not: not}
	for {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		expr.Values = append(expr.Values, value)
		t := p.next()
		if t == nil {
			return nil, errors.New("cql: unterminated value list")
		}
		if t.kind == "rparen" {
			break
		}
		if t.kind != "comma" {
			return nil, fmt.Errorf("cql: unexpected token %q in value list", t.text)
		}
	}
	return expr, nil
}

func (p *parser) parseBetween(prop string, not bool) (Expr, error) {
	lo, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if !p.matchKeyword("and") {
		return nil, errors.New("cql: between requires and")
	}
	hi, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if valueKind(lo) != valueKind(hi) {
		return nil, errors.New("cql: between bounds must share a type")
	}
	return &Between{Prop: prop, Lo: lo, Hi: hi, Not: not}, nil
}

func (p *parser) parseValue() (any, error) {
	t := p.next()
	if t == nil {
		return nil, errors.New("cql: expected value")
	}
	switch t.kind {
	case "string", "number", "bool":
		return t.value, nil
	default:
		return nil, fmt.Errorf("cql: expected value, got %q", t.text)
	}
}

func valueKind(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case int, int64, float64:
		return "number"
	default:
		return "string"
	}
}
