package chunk

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

type Column struct {
	Name string
	Type flattypes.ColumnType
}

func ColumnTypes(cols []Column) map[string]flattypes.ColumnType {
	types := make(map[string]flattypes.ColumnType, len(cols))
	for _, col := range cols {
		types[col.Name] = col.Type
	}
	return types
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

func GeoJSONFeature(header *flattypes.Header, f *flattypes.Feature) *Feature {
	var raw flattypes.Geometry
	props := make(map[string]any)
	for name, value := range DecodePropertyText(f.PropertiesBytes(), HeaderColumns(header)) {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		var decoded any
		if decoder.Decode(&decoded) == nil {
			props[name] = decoded
		}
	}
	return &Feature{
		Type:       "Feature",
		Geometry:   decodeGeometry(f.Geometry(&raw), header.GeometryType()),
		Properties: props,
	}
}

func DecodePropertyText(buf []byte, cols []Column) map[string]json.RawMessage {
	if len(buf) == 0 || len(cols) == 0 {
		return nil
	}
	props := make(map[string]json.RawMessage, len(cols))
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
			props[col.Name] = boolText(buf[off] != 0)
			off++
		case flattypes.ColumnTypeByte:
			if off+1 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatInt(int64(int8(buf[off])), 10))
			off++
		case flattypes.ColumnTypeUByte:
			if off+1 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatUint(uint64(buf[off]), 10))
			off++
		case flattypes.ColumnTypeShort:
			if off+2 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatInt(int64(int16(binary.LittleEndian.Uint16(buf[off:]))), 10))
			off += 2
		case flattypes.ColumnTypeUShort:
			if off+2 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatUint(uint64(binary.LittleEndian.Uint16(buf[off:])), 10))
			off += 2
		case flattypes.ColumnTypeInt:
			if off+4 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(buf[off:]))), 10))
			off += 4
		case flattypes.ColumnTypeUInt:
			if off+4 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(buf[off:])), 10))
			off += 4
		case flattypes.ColumnTypeLong:
			if off+8 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatInt(int64(binary.LittleEndian.Uint64(buf[off:])), 10))
			off += 8
		case flattypes.ColumnTypeULong:
			if off+8 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatUint(binary.LittleEndian.Uint64(buf[off:]), 10))
			off += 8
		case flattypes.ColumnTypeFloat:
			if off+4 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[off:]))), 'g', -1, 32))
			off += 4
		case flattypes.ColumnTypeDouble:
			if off+8 > len(buf) {
				return props
			}
			props[col.Name] = json.RawMessage(strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(buf[off:])), 'g', -1, 64))
			off += 8
		case flattypes.ColumnTypeString, flattypes.ColumnTypeDateTime:
			s, n, ok := stringValue(buf, off)
			if !ok {
				return props
			}
			props[col.Name] = quoteText(s)
			off += n
		case flattypes.ColumnTypeJson:
			s, n, ok := stringValue(buf, off)
			if !ok {
				return props
			}
			if json.Valid([]byte(s)) {
				props[col.Name] = json.RawMessage(s)
			} else {
				props[col.Name] = quoteText(s)
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
			props[col.Name] = quoteText(base64.StdEncoding.EncodeToString(buf[off+4 : off+4+l]))
			off += 4 + l
		default:
			return props
		}
	}
	return props
}

var (
	boolTrue  = json.RawMessage("true")
	boolFalse = json.RawMessage("false")
)

func boolText(v bool) json.RawMessage {
	if v {
		return boolTrue
	}
	return boolFalse
}

func quoteText(s string) json.RawMessage {
	simple := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			simple = false
			break
		}
	}
	if !simple {
		out, _ := json.Marshal(s)
		return out
	}
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	out = append(out, s...)
	return append(out, '"')
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
