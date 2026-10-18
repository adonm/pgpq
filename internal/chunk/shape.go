package chunk

import (
	"errors"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/paulmach/orb"
)

func ParseFlat(header *flattypes.Header, f *flattypes.Feature) (*ParsedGeometry, error) {
	var raw flattypes.Geometry
	g := f.Geometry(&raw)
	if g == nil {
		return nil, nil
	}
	typ := g.Type()
	if typ == flattypes.GeometryTypeUnknown {
		typ = header.GeometryType()
	}
	switch typ {
	case flattypes.GeometryTypePoint:
		if g.XyLength() < 2 {
			return nil, errors.New("chunk: invalid position")
		}
		return &ParsedGeometry{typ: typ, xy: []float64{g.Xy(0), g.Xy(1)}}, nil
	case flattypes.GeometryTypeLineString:
		return &ParsedGeometry{typ: typ, xy: flatPairs(g)}, nil
	case flattypes.GeometryTypePolygon:
		xy := flatPairs(g)
		return &ParsedGeometry{typ: typ, xy: xy, ends: flatEnds(g, len(xy)/2)}, nil
	case flattypes.GeometryTypeMultiPoint:
		if g.PartsLength() == 0 {
			return &ParsedGeometry{typ: typ, xy: flatPairs(g)}, nil
		}
		xy := make([]float64, 0, g.PartsLength()*2)
		for i := 0; i < g.PartsLength(); i++ {
			var part flattypes.Geometry
			if !g.Parts(&part, i) {
				break
			}
			if part.XyLength() >= 2 {
				xy = append(xy, part.Xy(0), part.Xy(1))
			}
		}
		return &ParsedGeometry{typ: typ, xy: xy}, nil
	case flattypes.GeometryTypeMultiLineString:
		if g.PartsLength() == 0 {
			return &ParsedGeometry{typ: typ, parts: []ParsedGeometry{{xy: flatPairs(g)}}}, nil
		}
		parts := make([]ParsedGeometry, 0, g.PartsLength())
		for i := 0; i < g.PartsLength(); i++ {
			var part flattypes.Geometry
			if !g.Parts(&part, i) {
				break
			}
			parts = append(parts, ParsedGeometry{xy: flatPairs(&part)})
		}
		return &ParsedGeometry{typ: typ, parts: parts}, nil
	case flattypes.GeometryTypeMultiPolygon:
		parts := make([]ParsedGeometry, 0, g.PartsLength())
		for i := 0; i < g.PartsLength(); i++ {
			var part flattypes.Geometry
			if !g.Parts(&part, i) {
				break
			}
			xy := flatPairs(&part)
			parts = append(parts, ParsedGeometry{xy: xy, ends: flatEnds(&part, len(xy)/2)})
		}
		return &ParsedGeometry{typ: typ, parts: parts}, nil
	case flattypes.GeometryTypeGeometryCollection:
		return nil, errors.New("chunk: geometry collections are not supported")
	}
	return nil, nil
}

func FromOrb(g orb.Geometry) (*ParsedGeometry, bool) {
	switch geom := g.(type) {
	case orb.Point:
		return &ParsedGeometry{typ: flattypes.GeometryTypePoint, xy: []float64{geom[0], geom[1]}}, true
	case orb.MultiPoint:
		xy := make([]float64, 0, len(geom)*2)
		for _, p := range geom {
			xy = append(xy, p[0], p[1])
		}
		return &ParsedGeometry{typ: flattypes.GeometryTypeMultiPoint, xy: xy}, true
	case orb.LineString:
		return &ParsedGeometry{typ: flattypes.GeometryTypeLineString, xy: lineXY(geom)}, true
	case orb.MultiLineString:
		parts := make([]ParsedGeometry, 0, len(geom))
		for _, line := range geom {
			parts = append(parts, ParsedGeometry{xy: lineXY(line)})
		}
		return &ParsedGeometry{typ: flattypes.GeometryTypeMultiLineString, parts: parts}, true
	case orb.Polygon:
		xy, ends := polyXY(geom)
		return &ParsedGeometry{typ: flattypes.GeometryTypePolygon, xy: xy, ends: ends}, true
	case orb.MultiPolygon:
		parts := make([]ParsedGeometry, 0, len(geom))
		for _, poly := range geom {
			xy, ends := polyXY(poly)
			parts = append(parts, ParsedGeometry{xy: xy, ends: ends})
		}
		return &ParsedGeometry{typ: flattypes.GeometryTypeMultiPolygon, parts: parts}, true
	}
	return nil, false
}

func (p *ParsedGeometry) Transform(fn func(x, y, z float64) (float64, float64, float64, error)) error {
	for i := 0; i+1 < len(p.xy); i += 2 {
		x, y, _, err := fn(p.xy[i], p.xy[i+1], 0)
		if err != nil {
			return err
		}
		p.xy[i], p.xy[i+1] = x, y
	}
	for i := range p.parts {
		if err := p.parts[i].Transform(fn); err != nil {
			return err
		}
	}
	return nil
}

func (p *ParsedGeometry) Estimate() int {
	n := 8*len(p.xy) + 4*len(p.ends)
	for i := range p.parts {
		n += p.parts[i].Estimate()
	}
	return n
}

func flatPairs(g *flattypes.Geometry) []float64 {
	n := g.XyLength() / 2
	out := make([]float64, 0, n*2)
	for i := 0; i < n; i++ {
		out = append(out, g.Xy(i*2), g.Xy(i*2+1))
	}
	return out
}

func flatEnds(g *flattypes.Geometry, total int) []uint32 {
	if g.EndsLength() == 0 {
		return []uint32{uint32(total)}
	}
	ends := make([]uint32, 0, g.EndsLength())
	for i := 0; i < g.EndsLength(); i++ {
		ends = append(ends, g.Ends(i))
	}
	return ends
}

func lineXY(line orb.LineString) []float64 {
	xy := make([]float64, 0, len(line)*2)
	for _, p := range line {
		xy = append(xy, p[0], p[1])
	}
	return xy
}

func polyXY(poly orb.Polygon) ([]float64, []uint32) {
	var xy []float64
	ends := make([]uint32, 0, len(poly))
	var count uint32
	for _, ring := range poly {
		xy = append(xy, lineXY(orb.LineString(ring))...)
		count += uint32(len(ring))
		ends = append(ends, count)
	}
	return xy, ends
}
