package geo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func ParseWKT(input string) (Shape, error) {
	p := &wktParser{input: input}
	p.skipSpace()
	name := p.ident()
	var shape Shape
	switch strings.ToUpper(name) {
	case "POLYGON":
		poly, err := p.polygon()
		if err != nil {
			return nil, err
		}
		shape = Shape{poly}
	case "MULTIPOLYGON":
		p.skipSpace()
		if !p.take('(') {
			return nil, errors.New("geo: expected ( after MULTIPOLYGON")
		}
		for {
			poly, err := p.polygon()
			if err != nil {
				return nil, err
			}
			shape = append(shape, poly)
			p.skipSpace()
			if p.take(',') {
				continue
			}
			break
		}
		if !p.take(')') {
			return nil, errors.New("geo: expected ) after multipolygon rings")
		}
	default:
		return nil, fmt.Errorf("geo: only POLYGON and MULTIPOLYGON filter geometries are supported, got %q", name)
	}
	p.skipSpace()
	if p.pos != len(p.input) {
		return nil, errors.New("geo: trailing input after geometry")
	}
	return shape, nil
}

func ParseGeoJSON(raw []byte) (Shape, error) {
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("geo: %w", err)
	}
	switch strings.ToLower(g.Type) {
	case "polygon":
		var rings [][][]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil {
			return nil, fmt.Errorf("geo: %w", err)
		}
		return Shape{ringsToPolygon(rings)}, nil
	case "multipolygon":
		var polys [][][][]float64
		if err := json.Unmarshal(g.Coordinates, &polys); err != nil {
			return nil, fmt.Errorf("geo: %w", err)
		}
		out := make(Shape, 0, len(polys))
		for _, rings := range polys {
			out = append(out, ringsToPolygon(rings))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("geo: only Polygon and MultiPolygon filter geometries are supported, got %q", g.Type)
	}
}

func ringsToPolygon(rings [][][]float64) Polygon {
	poly := make(Polygon, 0, len(rings))
	for _, ring := range rings {
		r := make(Ring, 0, len(ring))
		for _, pos := range ring {
			if len(pos) < 2 {
				continue
			}
			r = append(r, Point{X: pos[0], Y: pos[1]})
		}
		poly = append(poly, r)
	}
	return poly
}

type wktParser struct {
	input string
	pos   int
}

func (p *wktParser) skipSpace() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t' || p.input[p.pos] == '\n' || p.input[p.pos] == '\r') {
		p.pos++
	}
}

func (p *wktParser) take(c byte) bool {
	p.skipSpace()
	if p.pos < len(p.input) && p.input[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

func (p *wktParser) ident() string {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.input) && (p.input[p.pos] == '_' || p.input[p.pos] >= 'A' && p.input[p.pos] <= 'Z' || p.input[p.pos] >= 'a' && p.input[p.pos] <= 'z') {
		p.pos++
	}
	return p.input[start:p.pos]
}

func (p *wktParser) number() (float64, error) {
	p.skipSpace()
	start := p.pos
	if p.pos < len(p.input) && (p.input[p.pos] == '-' || p.input[p.pos] == '+') {
		p.pos++
	}
	for p.pos < len(p.input) && (p.input[p.pos] >= '0' && p.input[p.pos] <= '9' || p.input[p.pos] == '.') {
		p.pos++
	}
	if p.pos < len(p.input) && (p.input[p.pos] == 'e' || p.input[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.input) && (p.input[p.pos] == '-' || p.input[p.pos] == '+') {
			p.pos++
		}
		for p.pos < len(p.input) && p.input[p.pos] >= '0' && p.input[p.pos] <= '9' {
			p.pos++
		}
	}
	value, err := strconv.ParseFloat(p.input[start:p.pos], 64)
	if err != nil {
		return 0, fmt.Errorf("geo: invalid coordinate %q", p.input[start:p.pos])
	}
	return value, nil
}

func (p *wktParser) position() (Point, error) {
	x, err := p.number()
	if err != nil {
		return Point{}, err
	}
	y, err := p.number()
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func (p *wktParser) ring() (Ring, error) {
	if !p.take('(') {
		return nil, errors.New("geo: expected ( before ring")
	}
	var ring Ring
	for {
		pt, err := p.position()
		if err != nil {
			return nil, err
		}
		ring = append(ring, pt)
		if p.take(',') {
			continue
		}
		break
	}
	if !p.take(')') {
		return nil, errors.New("geo: expected ) after ring")
	}
	return ring, nil
}

func (p *wktParser) polygon() (Polygon, error) {
	if !p.take('(') {
		return nil, errors.New("geo: expected ( before polygon")
	}
	var poly Polygon
	for {
		ring, err := p.ring()
		if err != nil {
			return nil, err
		}
		poly = append(poly, ring)
		if p.take(',') {
			continue
		}
		break
	}
	if !p.take(')') {
		return nil, errors.New("geo: expected ) after polygon")
	}
	return poly, nil
}
