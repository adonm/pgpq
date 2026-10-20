package layer

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/adonm/postfgb/internal/chunk"
)

var denseCols = []chunk.Column{
	{Name: "id", Type: flattypes.ColumnTypeString},
	{Name: "name", Type: flattypes.ColumnTypeString},
	{Name: "n", Type: flattypes.ColumnTypeLong},
	{Name: "score", Type: flattypes.ColumnTypeDouble},
	{Name: "flag", Type: flattypes.ColumnTypeBool},
}

func testHeader(cols []chunk.Column, geometry flattypes.GeometryType) *flattypes.Header {
	b := flatbuffers.NewBuilder(0)
	h := writer.NewHeader(b).
		SetName("test").
		SetFeaturesCount(1).
		SetIndexNodeSize(16).
		SetGeometryType(geometry).
		SetCrs(writer.NewCrs(b).SetOrg("EPSG").SetCode(4326).SetCodeString("EPSG:4326").SetName("WGS 84"))
	wcols := make([]*writer.Column, len(cols))
	for i, col := range cols {
		wcols[i] = writer.NewColumn(b).SetName(col.Name).SetType(col.Type)
	}
	h.SetColumns(wcols)
	off := h.Build()
	b.FinishSizePrefixed(off)
	return flattypes.GetSizePrefixedRootAsHeader(b.FinishedBytes(), 0)
}

func testPoint(cols []chunk.Column, props map[string]json.RawMessage, x, y float64) chunk.InputFeature {
	b := flatbuffers.NewBuilder(0)
	g := writer.NewGeometry(b).SetType(flattypes.GeometryTypePoint).SetXY([]float64{x, y})
	f := writer.NewFeature(b).SetGeometry(g).SetProperties(encodeTestProps(cols, props))
	off := f.Build()
	b.FinishSizePrefixed(off)
	raw := b.FinishedBytes()
	return chunk.InputFeature{Raw: raw, Bounds: [4]float64{x, y, x, y}, Properties: props}
}

func testPolygon(cols []chunk.Column, props map[string]json.RawMessage, xy []float64, ends []uint32) chunk.InputFeature {
	b := flatbuffers.NewBuilder(0)
	g := writer.NewGeometry(b).SetType(flattypes.GeometryTypePolygon).SetXY(xy).SetEnds(ends)
	f := writer.NewFeature(b).SetGeometry(g).SetProperties(encodeTestProps(cols, props))
	off := f.Build()
	b.FinishSizePrefixed(off)
	raw := b.FinishedBytes()
	bounds := [4]float64{xy[0], xy[1], xy[0], xy[1]}
	for i := 0; i+1 < len(xy); i += 2 {
		bounds[0] = min(bounds[0], xy[i])
		bounds[1] = min(bounds[1], xy[i+1])
		bounds[2] = max(bounds[2], xy[i])
		bounds[3] = max(bounds[3], xy[i+1])
	}
	return chunk.InputFeature{Raw: raw, Bounds: bounds, Properties: props}
}

func encodeTestProps(cols []chunk.Column, props map[string]json.RawMessage) []byte {
	out := make([]byte, 0, 256)
	for i, col := range cols {
		raw, ok := props[col.Name]
		if !ok {
			continue
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(i))
		switch col.Type {
		case flattypes.ColumnTypeBool:
			if string(raw) == "true" {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case flattypes.ColumnTypeLong:
			n, _ := strconv.ParseInt(string(raw), 10, 64)
			out = binary.LittleEndian.AppendUint64(out, uint64(n))
		case flattypes.ColumnTypeDouble:
			f, _ := strconv.ParseFloat(string(raw), 64)
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(f))
		default:
			var text string
			if len(raw) > 0 && raw[0] == '"' {
				json.Unmarshal(raw, &text)
			} else {
				text = string(raw)
			}
			out = binary.LittleEndian.AppendUint32(out, uint32(len(text)))
			out = append(out, text...)
		}
	}
	return out
}
