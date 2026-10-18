package chunk

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"
)

type RawGeometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

type InputFeature struct {
	Geometry   RawGeometry
	Properties map[string]json.RawMessage
}

type parsedGeometry struct {
	typ   flattypes.GeometryType
	xy    []float64
	ends  []uint32
	parts []parsedGeometry
}

var geometryTypes = map[string]flattypes.GeometryType{
	"Point":           flattypes.GeometryTypePoint,
	"LineString":      flattypes.GeometryTypeLineString,
	"Polygon":         flattypes.GeometryTypePolygon,
	"MultiPoint":      flattypes.GeometryTypeMultiPoint,
	"MultiLineString": flattypes.GeometryTypeMultiLineString,
	"MultiPolygon":    flattypes.GeometryTypeMultiPolygon,
}

func Encode(name string, features []InputFeature) ([]byte, error) {
	return EncodeCols(name, inferColumns(features), features)
}

func EncodeCols(name string, cols []Column, features []InputFeature) ([]byte, error) {
	if len(features) == 0 {
		return nil, errors.New("chunk: no features to encode")
	}
	parsed := make([]*parsedGeometry, len(features))
	for i := range features {
		g, err := parseGeometry(&features[i].Geometry)
		if err != nil {
			return nil, err
		}
		parsed[i] = g
	}
	headerBuilder := flatbuffers.NewBuilder(0)
	header := writer.NewHeader(headerBuilder).
		SetName(name).
		SetGeometryType(geometryType(features)).
		SetCrs(writer.NewCrs(headerBuilder).
			SetOrg("EPSG").
			SetCode(4326).
			SetCodeString("EPSG:4326").
			SetName("WGS 84"))
	wcols := make([]*writer.Column, len(cols))
	for i, col := range cols {
		wcols[i] = writer.NewColumn(headerBuilder).SetName(col.Name).SetType(col.Type)
	}
	header.SetColumns(wcols)
	gen := &generator{features: features, parsed: parsed, cols: cols}
	w := writer.NewWriter(header, true, gen, nil, writer.WithMemory())
	var buf bytes.Buffer
	if _, err := w.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type generator struct {
	features []InputFeature
	parsed   []*parsedGeometry
	cols     []Column
	next     int
}

func (g *generator) Generate() *writer.Feature {
	if g.next >= len(g.features) {
		return nil
	}
	f := g.features[g.next]
	parsed := g.parsed[g.next]
	g.next++
	builder := flatbuffers.NewBuilder(0)
	return writer.NewFeature(builder).
		SetGeometry(buildGeometry(builder, parsed)).SetProperties(encodeProperties(g.cols, f.Properties))
}

func buildGeometry(b *flatbuffers.Builder, g *parsedGeometry) *writer.Geometry {
	if g.parts == nil {
		wg := writer.NewGeometry(b).SetType(g.typ)
		if len(g.xy) > 0 {
			wg.SetXY(g.xy)
		}
		if len(g.ends) > 0 {
			wg.SetEnds(g.ends)
		}
		return wg
	}
	parts := make([]writer.Geometry, len(g.parts))
	for i := range g.parts {
		parts[i] = *buildGeometry(b, &g.parts[i])
	}
	return writer.NewGeometry(b).SetType(g.typ).SetParts(parts)
}

func geometryType(features []InputFeature) flattypes.GeometryType {
	t, ok := geometryTypes[features[0].Geometry.Type]
	if !ok {
		return flattypes.GeometryTypeUnknown
	}
	for _, f := range features[1:] {
		if geometryTypes[f.Geometry.Type] != t {
			return flattypes.GeometryTypeUnknown
		}
	}
	return t
}

func inferColumns(features []InputFeature) []Column {
	types := map[string]flattypes.ColumnType{}
	for _, f := range features {
		for name, raw := range f.Properties {
			if _, seen := types[name]; seen {
				continue
			}
			if t, ok := columnType(raw); ok {
				types[name] = t
			}
		}
	}
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	cols := make([]Column, 0, len(names))
	for _, name := range names {
		cols = append(cols, Column{Name: name, Type: types[name]})
	}
	return cols
}

func columnType(raw json.RawMessage) (flattypes.ColumnType, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	switch raw[0] {
	case '"':
		return flattypes.ColumnTypeString, true
	case 't', 'f':
		return flattypes.ColumnTypeBool, true
	case '{', '[':
		return flattypes.ColumnTypeJson, true
	default:
		if strings.ContainsAny(string(raw), ".eE") {
			return flattypes.ColumnTypeDouble, true
		}
		return flattypes.ColumnTypeLong, true
	}
}

func encodeProperties(cols []Column, props map[string]json.RawMessage) []byte {
	var buf []byte
	for i, col := range cols {
		raw, ok := props[col.Name]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		val := propertyValue(col.Type, raw)
		if val == nil {
			continue
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(i))
		buf = append(buf, val...)
	}
	return buf
}

func propertyValue(t flattypes.ColumnType, raw json.RawMessage) []byte {
	switch t {
	case flattypes.ColumnTypeString, flattypes.ColumnTypeDateTime:
		return lengthPrefixed(unquote(raw))
	case flattypes.ColumnTypeJson:
		return lengthPrefixed(raw)
	case flattypes.ColumnTypeBinary:
		data, err := base64.StdEncoding.DecodeString(string(unquote(raw)))
		if err != nil {
			return nil
		}
		return lengthPrefixed(data)
	}
	return scalarValue(t, string(unquote(raw)))
}

func scalarValue(t flattypes.ColumnType, text string) []byte {
	switch t {
	case flattypes.ColumnTypeBool:
		switch text {
		case "true":
			return []byte{1}
		case "false":
			return []byte{0}
		}
	case flattypes.ColumnTypeByte:
		return intValue(text, 8, true)
	case flattypes.ColumnTypeUByte:
		return intValue(text, 8, false)
	case flattypes.ColumnTypeShort:
		return intValue(text, 16, true)
	case flattypes.ColumnTypeUShort:
		return intValue(text, 16, false)
	case flattypes.ColumnTypeInt:
		return intValue(text, 32, true)
	case flattypes.ColumnTypeUInt:
		return intValue(text, 32, false)
	case flattypes.ColumnTypeLong:
		return intValue(text, 64, true)
	case flattypes.ColumnTypeULong:
		return intValue(text, 64, false)
	case flattypes.ColumnTypeFloat:
		if f, err := strconv.ParseFloat(text, 32); err == nil {
			return binary.LittleEndian.AppendUint32(nil, math.Float32bits(float32(f)))
		}
	case flattypes.ColumnTypeDouble:
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return binary.LittleEndian.AppendUint64(nil, math.Float64bits(f))
		}
	}
	return nil
}

func intValue(text string, bits int, signed bool) []byte {
	var n uint64
	var err error
	if signed {
		var v int64
		v, err = strconv.ParseInt(text, 10, bits)
		n = uint64(v)
	} else {
		n, err = strconv.ParseUint(text, 10, bits)
	}
	if err != nil {
		return nil
	}
	return binary.LittleEndian.AppendUint64(nil, n)[:bits/8]
}

func unquote(raw []byte) []byte {
	if len(raw) > 1 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []byte(s)
		}
	}
	return raw
}

func lengthPrefixed(b []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

func parseGeometry(g *RawGeometry) (*parsedGeometry, error) {
	var coords any
	if len(g.Coordinates) > 0 {
		if err := json.Unmarshal(g.Coordinates, &coords); err != nil {
			return nil, fmt.Errorf("chunk: %s coordinates: %w", g.Type, err)
		}
	}
	switch g.Type {
	case "Point":
		xy, err := flatPosition(coords)
		if err != nil {
			return nil, err
		}
		return &parsedGeometry{typ: flattypes.GeometryTypePoint, xy: xy}, nil
	case "LineString":
		xy, err := flatPositions(coords)
		if err != nil {
			return nil, err
		}
		return &parsedGeometry{typ: flattypes.GeometryTypeLineString, xy: xy}, nil
	case "Polygon":
		xy, ends, err := flatRings(coords)
		if err != nil {
			return nil, err
		}
		return &parsedGeometry{typ: flattypes.GeometryTypePolygon, xy: xy, ends: ends}, nil
	case "MultiPoint":
		xy, err := flatPositions(coords)
		if err != nil {
			return nil, err
		}
		return &parsedGeometry{typ: flattypes.GeometryTypeMultiPoint, xy: xy}, nil
	case "MultiLineString":
		lines, err := flatMulti(coords)
		if err != nil {
			return nil, err
		}
		parts := make([]parsedGeometry, len(lines))
		for i, line := range lines {
			parts[i] = parsedGeometry{xy: line}
		}
		return &parsedGeometry{typ: flattypes.GeometryTypeMultiLineString, parts: parts}, nil
	case "MultiPolygon":
		polys, ok := coords.([]any)
		if !ok {
			return nil, errors.New("chunk: multipolygon coordinates are not a polygon list")
		}
		parts := make([]parsedGeometry, len(polys))
		for i, poly := range polys {
			xy, ends, err := flatRings(poly)
			if err != nil {
				return nil, err
			}
			parts[i] = parsedGeometry{xy: xy, ends: ends}
		}
		return &parsedGeometry{typ: flattypes.GeometryTypeMultiPolygon, parts: parts}, nil
	default:
		return nil, fmt.Errorf("chunk: unsupported geometry type %q", g.Type)
	}
}

func flatPosition(v any) ([]float64, error) {
	pos, ok := v.([]any)
	if !ok || len(pos) < 2 {
		return nil, errors.New("chunk: invalid position")
	}
	x, okX := pos[0].(float64)
	y, okY := pos[1].(float64)
	if !okX || !okY {
		return nil, errors.New("chunk: non-numeric position")
	}
	return []float64{x, y}, nil
}

func flatPositions(v any) ([]float64, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("chunk: coordinates are not a position list")
	}
	out := make([]float64, 0, len(items)*2)
	for _, item := range items {
		pos, err := flatPosition(item)
		if err != nil {
			return nil, err
		}
		out = append(out, pos...)
	}
	return out, nil
}

func flatRings(v any) ([]float64, []uint32, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, nil, errors.New("chunk: polygon coordinates are not a ring list")
	}
	var xy []float64
	var ends []uint32
	var count uint32
	for _, item := range items {
		ring, err := flatPositions(item)
		if err != nil {
			return nil, nil, err
		}
		xy = append(xy, ring...)
		count += uint32(len(ring) / 2)
		ends = append(ends, count)
	}
	return xy, ends, nil
}

func flatMulti(v any) ([][]float64, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("chunk: multi geometry coordinates are not a list")
	}
	out := make([][]float64, 0, len(items))
	for _, item := range items {
		flat, err := flatPositions(item)
		if err != nil {
			return nil, err
		}
		out = append(out, flat)
	}
	return out, nil
}
