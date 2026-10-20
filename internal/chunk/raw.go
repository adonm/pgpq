package chunk

import (
	"encoding/json"
	"errors"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/adonm/postfgb/internal/fgb"
)

type InputFeature struct {
	Raw        []byte
	Bounds     [4]float64
	Properties map[string]json.RawMessage
}

func Bounds(header *flattypes.Header, f *flattypes.Feature) ([4]float64, bool) {
	var g flattypes.Geometry
	geom := f.Geometry(&g)
	if geom == nil {
		return [4]float64{}, false
	}
	typ := geom.Type()
	if typ == flattypes.GeometryTypeUnknown {
		typ = header.GeometryType()
	}
	var b [4]float64
	seen := false
	var walk func(*flattypes.Geometry)
	walk = func(g *flattypes.Geometry) {
		if g == nil {
			return
		}
		for i := 0; i+1 < g.XyLength(); i += 2 {
			x, y := g.Xy(i), g.Xy(i+1)
			if !seen {
				b[0], b[1], b[2], b[3] = x, y, x, y
				seen = true
				continue
			}
			if x < b[0] {
				b[0] = x
			}
			if y < b[1] {
				b[1] = y
			}
			if x > b[2] {
				b[2] = x
			}
			if y > b[3] {
				b[3] = y
			}
		}
		for i := 0; i < g.PartsLength(); i++ {
			var part flattypes.Geometry
			if g.Parts(&part, i) {
				walk(&part)
			}
		}
	}
	_ = typ
	walk(geom)
	return b, seen
}

func EncodeCell(name string, header *flattypes.Header, features []InputFeature, keys []uint64, ids []string) ([]byte, []fgb.FenceEntry, error) {
	if len(features) == 0 {
		return nil, nil, errors.New("chunk: no features to encode")
	}
	if len(keys) != len(features) || len(ids) != len(features) {
		return nil, nil, errors.New("chunk: keys and ids must match the feature count")
	}
	envelope := [4]float64{features[0].Bounds[0], features[0].Bounds[1], features[0].Bounds[2], features[0].Bounds[3]}
	offsets := make([]uint64, len(features))
	total := 0
	items := make([]fgb.Item, len(features))
	for i := range features {
		if len(features[i].Raw) < 4 {
			return nil, nil, errors.New("chunk: feature has no encoded bytes")
		}
		offsets[i] = uint64(total)
		items[i] = fgb.Item{BBox: features[i].Bounds, Offset: offsets[i]}
		total += len(features[i].Raw)
		if i > 0 {
			envelope[0] = min(envelope[0], features[i].Bounds[0])
			envelope[1] = min(envelope[1], features[i].Bounds[1])
			envelope[2] = max(envelope[2], features[i].Bounds[2])
			envelope[3] = max(envelope[3], features[i].Bounds[3])
		}
	}
	headerBytes := encodeHeader(name, header, len(features), envelope, 16)
	index := fgb.BuildIndex(items, 16)
	featuresAt := 8 + len(headerBytes) + len(index)
	data := make([]byte, 0, featuresAt+total)
	data = append(data, writer.MagicBytes...)
	data = append(data, headerBytes...)
	data = append(data, index...)
	for _, feature := range features {
		data = append(data, feature.Raw...)
	}
	fence := make([]fgb.FenceEntry, 0, (len(features)+fgb.FenceEvery-1)/fgb.FenceEvery)
	for i := 0; i < len(features); i += fgb.FenceEvery {
		fence = append(fence, fgb.FenceEntry{Key: keys[i], ID: ids[i], Offset: uint64(featuresAt) + offsets[i]})
	}
	return data, fence, nil
}

func EncodePage(name string, header *flattypes.Header, features []InputFeature) ([]byte, error) {
	if len(features) == 0 {
		return nil, errors.New("chunk: no features to encode")
	}
	headerBytes := encodeHeader(name, header, len(features), [4]float64{}, 0)
	data := make([]byte, 0, 8+len(headerBytes))
	data = append(data, writer.MagicBytes...)
	data = append(data, headerBytes...)
	for _, feature := range features {
		data = append(data, feature.Raw...)
	}
	return data, nil
}

func encodeHeader(name string, source *flattypes.Header, count int, envelope [4]float64, nodeSize uint16) []byte {
	builder := flatbuffers.NewBuilder(0)
	header := writer.NewHeader(builder).
		SetName(name).
		SetFeaturesCount(uint64(count)).
		SetIndexNodeSize(nodeSize)
	if nodeSize > 0 {
		header.SetEnvelope(envelope[:])
	}
	if source != nil {
		header.SetGeometryType(source.GeometryType())
		if source.ColumnsLength() > 0 {
			cols := make([]*writer.Column, 0, source.ColumnsLength())
			for i := 0; i < source.ColumnsLength(); i++ {
				var col flattypes.Column
				if !source.Columns(&col, i) {
					break
				}
				cols = append(cols, writer.NewColumn(builder).SetName(string(col.Name())).SetType(col.Type()))
			}
			header.SetColumns(cols)
		}
	}
	crs := writer.NewCrs(builder).SetOrg("EPSG").SetCode(4326).SetCodeString("EPSG:4326").SetName("WGS 84")
	if source != nil {
		var sourceCRS flattypes.Crs
		if source.Crs(&sourceCRS) != nil {
			crs.SetOrg(string(sourceCRS.Org())).SetCode(sourceCRS.Code()).SetCodeString(string(sourceCRS.CodeString())).SetName(string(sourceCRS.Name()))
		}
	}
	header.SetCrs(crs)
	offset := header.Build()
	builder.FinishSizePrefixed(offset)
	return builder.FinishedBytes()
}
