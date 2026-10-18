package chunk

import (
	"bytes"
	"errors"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"
)

func (c *Chunk) Header() *flattypes.Header {
	return c.header
}

func EncodeFeatures(name string, header *flattypes.Header, geometry flattypes.GeometryType, features []*flattypes.Feature) ([]byte, error) {
	if len(features) == 0 {
		return nil, errors.New("chunk: no features to encode")
	}
	headerBuilder := flatbuffers.NewBuilder(0)
	wheader := writer.NewHeader(headerBuilder).
		SetName(name).
		SetGeometryType(geometry)
	if n := header.ColumnsLength(); n > 0 {
		cols := make([]*writer.Column, 0, n)
		for i := 0; i < n; i++ {
			var col flattypes.Column
			if !header.Columns(&col, i) {
				break
			}
			cols = append(cols, writer.NewColumn(headerBuilder).SetName(string(col.Name())).SetType(col.Type()))
		}
		wheader.SetColumns(cols)
	}
	gen := &copyGenerator{features: features}
	w := writer.NewWriter(wheader, true, gen, nil, writer.WithMemory())
	var buf bytes.Buffer
	if _, err := w.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type copyGenerator struct {
	features []*flattypes.Feature
	next     int
}

func (g *copyGenerator) Generate() *writer.Feature {
	if g.next >= len(g.features) {
		return nil
	}
	f := g.features[g.next]
	g.next++
	builder := flatbuffers.NewBuilder(0)
	return writer.NewFeature(builder).
		SetGeometry(geometryFromFlattypes(builder, f)).
		SetProperties(f.PropertiesBytes())
}

func geometryFromFlattypes(b *flatbuffers.Builder, f *flattypes.Feature) *writer.Geometry {
	var raw flattypes.Geometry
	g := f.Geometry(&raw)
	wg := writer.NewGeometry(b)
	if g == nil {
		return wg.SetType(flattypes.GeometryTypeUnknown)
	}
	wg.SetType(g.Type())
	if n := g.XyLength(); n > 0 {
		xy := make([]float64, n)
		for i := range xy {
			xy[i] = g.Xy(i)
		}
		wg.SetXY(xy)
	}
	if n := g.EndsLength(); n > 0 {
		ends := make([]uint32, n)
		for i := range ends {
			ends[i] = g.Ends(i)
		}
		wg.SetEnds(ends)
	}
	if n := g.PartsLength(); n > 0 {
		parts := make([]writer.Geometry, n)
		for i := 0; i < n; i++ {
			var part flattypes.Geometry
			if !g.Parts(&part, i) {
				break
			}
			cp := part
			parts[i] = *copyGeometry(b, &cp)
		}
		wg.SetParts(parts)
	}
	return wg
}

func copyGeometry(b *flatbuffers.Builder, g *flattypes.Geometry) *writer.Geometry {
	wg := writer.NewGeometry(b).SetType(g.Type())
	if n := g.XyLength(); n > 0 {
		xy := make([]float64, n)
		for i := range xy {
			xy[i] = g.Xy(i)
		}
		wg.SetXY(xy)
	}
	if n := g.EndsLength(); n > 0 {
		ends := make([]uint32, n)
		for i := range ends {
			ends[i] = g.Ends(i)
		}
		wg.SetEnds(ends)
	}
	if n := g.PartsLength(); n > 0 {
		parts := make([]writer.Geometry, n)
		for i := 0; i < n; i++ {
			var part flattypes.Geometry
			if !g.Parts(&part, i) {
				break
			}
			cp := part
			parts[i] = *copyGeometry(b, &cp)
		}
		wg.SetParts(parts)
	}
	return wg
}
