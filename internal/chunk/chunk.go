package chunk

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"

	flatgeobuf "github.com/flatgeobuf/flatgeobuf/src/go"
	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

type Chunk struct {
	Data   []byte
	header *flattypes.Header
	fgb    *flatgeobuf.FlatGeoBuf
}

type Column struct {
	Name string
	Type flattypes.ColumnType
}

type Geometry struct {
	Type        string     `json:"type"`
	Coordinates any        `json:"coordinates,omitempty"`
	Geometries  []Geometry `json:"geometries,omitempty"`
}

type Feature struct {
	Type       string         `json:"type"`
	ID         any            `json:"id,omitempty"`
	Geometry   *Geometry      `json:"geometry"`
	Properties map[string]any `json:"properties,omitempty"`
}

func Open(data []byte) (*Chunk, error) {
	raw, err := Decompress(data)
	if err != nil {
		return nil, err
	}
	return FromRaw(raw)
}

func FromRaw(raw []byte) (*Chunk, error) {
	fgb, err := flatgeobuf.NewWithData(raw)
	if err != nil {
		return nil, err
	}
	return &Chunk{Data: raw, header: fgb.Header(), fgb: fgb}, nil
}

func (c *Chunk) Count() int {
	return int(c.header.FeaturesCount())
}

func (c *Chunk) Envelope() ([4]float64, bool) {
	var env [4]float64
	if c.header.EnvelopeLength() < 4 {
		return env, false
	}
	for i := range env {
		env[i] = c.header.Envelope(i)
	}
	return env, true
}

func HeaderColumns(header *flattypes.Header) []Column {
	n := header.ColumnsLength()
	cols := make([]Column, 0, n)
	for i := 0; i < n; i++ {
		var col flattypes.Column
		if !header.Columns(&col, i) {
			break
		}
		cols = append(cols, Column{Name: string(col.Name()), Type: col.Type()})
	}
	return cols
}

func (c *Chunk) Columns() []Column {
	return HeaderColumns(c.header)
}

func (c *Chunk) Search(minX, minY, maxX, maxY float64) ([]*flattypes.Feature, error) {
	return c.fgb.Search(minX, minY, maxX, maxY)
}

func (c *Chunk) SearchAll() ([]*flattypes.Feature, error) {
	env, ok := c.Envelope()
	if !ok {
		return nil, errors.New("chunk: missing envelope")
	}
	return c.Search(env[0], env[1], env[2], env[3])
}

func GeoJSONFeature(header *flattypes.Header, f *flattypes.Feature) *Feature {
	var raw flattypes.Geometry
	return &Feature{
		Type:       "Feature",
		Geometry:   decodeGeometry(f.Geometry(&raw), header.GeometryType()),
		Properties: decodeProperties(f.PropertiesBytes(), HeaderColumns(header)),
	}
}

func (c *Chunk) GeoJSON(f *flattypes.Feature) *Feature {
	return GeoJSONFeature(c.header, f)
}

func decodeGeometry(g *flattypes.Geometry, fallback flattypes.GeometryType) *Geometry {
	if g == nil {
		return nil
	}
	typ := g.Type()
	if typ == flattypes.GeometryTypeUnknown {
		typ = fallback
	}
	switch typ {
	case flattypes.GeometryTypePoint:
		return &Geometry{Type: "Point", Coordinates: position(g, 0)}
	case flattypes.GeometryTypeLineString:
		return &Geometry{Type: "LineString", Coordinates: pairs(g)}
	case flattypes.GeometryTypePolygon:
		return &Geometry{Type: "Polygon", Coordinates: rings(g)}
	case flattypes.GeometryTypeMultiPoint:
		return &Geometry{Type: "MultiPoint", Coordinates: multiPoints(g)}
	case flattypes.GeometryTypeMultiLineString:
		return &Geometry{Type: "MultiLineString", Coordinates: multiLines(g)}
	case flattypes.GeometryTypeMultiPolygon:
		return &Geometry{Type: "MultiPolygon", Coordinates: multiPolygons(g)}
	case flattypes.GeometryTypeGeometryCollection:
		return &Geometry{Type: "GeometryCollection", Geometries: collection(g, fallback)}
	default:
		return nil
	}
}

func parts(g *flattypes.Geometry) []*flattypes.Geometry {
	n := g.PartsLength()
	out := make([]*flattypes.Geometry, 0, n)
	for i := 0; i < n; i++ {
		var p flattypes.Geometry
		if !g.Parts(&p, i) {
			break
		}
		copied := p
		out = append(out, &copied)
	}
	return out
}

func pairs(g *flattypes.Geometry) [][]float64 {
	n := g.XyLength() / 2
	out := make([][]float64, n)
	for i := 0; i < n; i++ {
		out[i] = position(g, i)
	}
	return out
}

func position(g *flattypes.Geometry, i int) []float64 {
	return []float64{g.Xy(i * 2), g.Xy(i*2 + 1)}
}

func rings(g *flattypes.Geometry) [][][]float64 {
	pts := pairs(g)
	if g.EndsLength() == 0 {
		return [][][]float64{pts}
	}
	var out [][][]float64
	start := 0
	for i := 0; i < g.EndsLength(); i++ {
		end := int(g.Ends(i))
		if end > len(pts) {
			end = len(pts)
		}
		out = append(out, pts[start:end])
		start = end
	}
	return out
}

func multiPoints(g *flattypes.Geometry) [][]float64 {
	if g.PartsLength() > 0 {
		ps := parts(g)
		out := make([][]float64, 0, len(ps))
		for _, p := range ps {
			out = append(out, position(p, 0))
		}
		return out
	}
	return pairs(g)
}

func multiLines(g *flattypes.Geometry) [][][]float64 {
	ps := parts(g)
	if len(ps) == 0 {
		return [][][]float64{pairs(g)}
	}
	out := make([][][]float64, 0, len(ps))
	for _, p := range ps {
		out = append(out, pairs(p))
	}
	return out
}

func multiPolygons(g *flattypes.Geometry) [][][][]float64 {
	ps := parts(g)
	out := make([][][][]float64, 0, len(ps))
	for _, p := range ps {
		out = append(out, rings(p))
	}
	return out
}

func collection(g *flattypes.Geometry, fallback flattypes.GeometryType) []Geometry {
	ps := parts(g)
	out := make([]Geometry, 0, len(ps))
	for _, p := range ps {
		if d := decodeGeometry(p, fallback); d != nil {
			out = append(out, *d)
		}
	}
	return out
}

func decodeProperties(buf []byte, cols []Column) map[string]any {
	if len(buf) == 0 || len(cols) == 0 {
		return nil
	}
	props := make(map[string]any, len(cols))
	off := 0
	for off+2 <= len(buf) {
		idx := int(binary.LittleEndian.Uint16(buf[off:]))
		off += 2
		if idx >= len(cols) {
			break
		}
		col := cols[idx]
		switch col.Type {
		case flattypes.ColumnTypeBool:
			if off+1 > len(buf) {
				return props
			}
			props[col.Name] = buf[off] != 0
			off++
		case flattypes.ColumnTypeByte:
			if off+1 > len(buf) {
				return props
			}
			props[col.Name] = int8(buf[off])
			off++
		case flattypes.ColumnTypeUByte:
			if off+1 > len(buf) {
				return props
			}
			props[col.Name] = buf[off]
			off++
		case flattypes.ColumnTypeShort:
			if off+2 > len(buf) {
				return props
			}
			props[col.Name] = int16(binary.LittleEndian.Uint16(buf[off:]))
			off += 2
		case flattypes.ColumnTypeUShort:
			if off+2 > len(buf) {
				return props
			}
			props[col.Name] = binary.LittleEndian.Uint16(buf[off:])
			off += 2
		case flattypes.ColumnTypeInt:
			if off+4 > len(buf) {
				return props
			}
			props[col.Name] = int32(binary.LittleEndian.Uint32(buf[off:]))
			off += 4
		case flattypes.ColumnTypeUInt:
			if off+4 > len(buf) {
				return props
			}
			props[col.Name] = binary.LittleEndian.Uint32(buf[off:])
			off += 4
		case flattypes.ColumnTypeLong:
			if off+8 > len(buf) {
				return props
			}
			props[col.Name] = int64(binary.LittleEndian.Uint64(buf[off:]))
			off += 8
		case flattypes.ColumnTypeULong:
			if off+8 > len(buf) {
				return props
			}
			props[col.Name] = binary.LittleEndian.Uint64(buf[off:])
			off += 8
		case flattypes.ColumnTypeFloat:
			if off+4 > len(buf) {
				return props
			}
			props[col.Name] = math.Float32frombits(binary.LittleEndian.Uint32(buf[off:]))
			off += 4
		case flattypes.ColumnTypeDouble:
			if off+8 > len(buf) {
				return props
			}
			props[col.Name] = math.Float64frombits(binary.LittleEndian.Uint64(buf[off:]))
			off += 8
		case flattypes.ColumnTypeString, flattypes.ColumnTypeDateTime:
			s, n, ok := stringValue(buf, off)
			if !ok {
				return props
			}
			props[col.Name] = s
			off += n
		case flattypes.ColumnTypeJson:
			s, n, ok := stringValue(buf, off)
			if !ok {
				return props
			}
			var v any
			if json.Unmarshal([]byte(s), &v) == nil {
				props[col.Name] = v
			} else {
				props[col.Name] = s
			}
			off += n
		case flattypes.ColumnTypeBinary:
			if off+4 > len(buf) {
				return props
			}
			l := int(binary.LittleEndian.Uint32(buf[off:]))
			if off+4+l > len(buf) {
				return props
			}
			props[col.Name] = append([]byte{}, buf[off+4:off+4+l]...)
			off += 4 + l
		default:
			return props
		}
	}
	return props
}

func stringValue(buf []byte, off int) (string, int, bool) {
	if off+4 > len(buf) {
		return "", 0, false
	}
	l := int(binary.LittleEndian.Uint32(buf[off:]))
	if off+4+l > len(buf) {
		return "", 0, false
	}
	return string(buf[off+4 : off+4+l]), 4 + l, true
}
