package cql

import (
	"errors"
	"fmt"
	"strings"

	"github.com/adonm/postfgb/internal/geo"
)

type Spatial struct {
	Fn    string
	Shape geo.Shape
}

func (e *Spatial) SQL(*Builder) string {
	return "true"
}

func ExtractSpatial(e Expr) (Expr, []*Spatial, error) {
	switch v := e.(type) {
	case nil:
		return nil, nil, nil
	case *Spatial:
		return nil, []*Spatial{v}, nil
	case andExpr:
		left, leftSpatials, err := ExtractSpatial(v.left)
		if err != nil {
			return nil, nil, err
		}
		right, rightSpatials, err := ExtractSpatial(v.right)
		if err != nil {
			return nil, nil, err
		}
		return And(left, right), append(leftSpatials, rightSpatials...), nil
	case orExpr:
		_, leftSpatials, err := ExtractSpatial(v.left)
		if err != nil {
			return nil, nil, err
		}
		_, rightSpatials, err := ExtractSpatial(v.right)
		if err != nil {
			return nil, nil, err
		}
		if len(leftSpatials)+len(rightSpatials) > 0 {
			return nil, nil, errors.New("cql: spatial predicates can only be combined with AND")
		}
		return v, nil, nil
	case notExpr:
		_, spatials, err := ExtractSpatial(v.inner)
		if err != nil {
			return nil, nil, err
		}
		if len(spatials) > 0 {
			return nil, nil, errors.New("cql: spatial predicates cannot be negated")
		}
		return v, nil, nil
	}
	return e, nil, nil
}

func (p *parser) peekAt(offset int) *token {
	if p.pos+offset >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos+offset]
}

func (p *parser) parseSpatial() (Expr, error) {
	name := p.tokens[p.pos].text
	p.pos++
	if t := p.next(); t == nil || t.kind != "lparen" {
		return nil, errors.New("cql: expected ( after spatial function")
	}
	if t := p.next(); t == nil || t.kind != "ident" {
		return nil, errors.New("cql: expected geometry operand in spatial function")
	}
	if t := p.next(); t == nil || t.kind != "comma" {
		return nil, errors.New("cql: expected , in spatial function")
	}
	literal := p.next()
	if literal == nil {
		return nil, errors.New("cql: expected geometry literal in spatial function")
	}
	var shape geo.Shape
	var err error
	if literal.kind == "string" {
		shape, err = geo.ParseWKT(literal.text)
	} else {
		start := p.pos - 1
		depth := 0
		ended := false
		for p.pos < len(p.tokens) {
			tok := p.tokens[p.pos]
			if tok.kind == "lparen" {
				depth++
			}
			if tok.kind == "rparen" {
				depth--
				if depth == 0 {
					p.pos++
					ended = true
					break
				}
			}
			p.pos++
		}
		if !ended {
			return nil, errors.New("cql: unterminated geometry literal")
		}
		parts := make([]string, 0, p.pos-start)
		for _, tok := range p.tokens[start:p.pos] {
			parts = append(parts, tok.text)
		}
		shape, err = geo.ParseWKT(strings.Join(parts, " "))
	}
	if err != nil {
		return nil, fmt.Errorf("cql: %w", err)
	}
	if t := p.next(); t == nil || t.kind != "rparen" {
		return nil, errors.New("cql: missing closing parenthesis in spatial function")
	}
	return &Spatial{Fn: strings.ToLower(name), Shape: shape}, nil
}

func spatialFunction(name string) bool {
	return strings.EqualFold(name, "s_intersects") || strings.EqualFold(name, "intersects")
}
