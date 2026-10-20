package fgb_test

import (
	"encoding/binary"
	"encoding/json"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/adonm/postfgb/internal/chunk"
)

func testHeader(cols []chunk.Column) *flattypes.Header {
	b := flatbuffers.NewBuilder(0)
	h := writer.NewHeader(b).
		SetName("test").
		SetFeaturesCount(1).
		SetIndexNodeSize(16).
		SetGeometryType(flattypes.GeometryTypePoint).
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
	rawProps := make([]byte, 0, 64)
	for i, col := range cols {
		value := props[col.Name]
		if value == nil {
			continue
		}
		rawProps = binary.LittleEndian.AppendUint16(rawProps, uint16(i))
		text := string(value)
		if len(value) > 1 && value[0] == '"' {
			json.Unmarshal(value, &text)
		}
		rawProps = binary.LittleEndian.AppendUint32(rawProps, uint32(len(text)))
		rawProps = append(rawProps, text...)
	}
	f := writer.NewFeature(b).SetGeometry(g).SetProperties(rawProps)
	off := f.Build()
	b.FinishSizePrefixed(off)
	return chunk.InputFeature{Raw: b.FinishedBytes(), Bounds: [4]float64{x, y, x, y}, Properties: props}
}
