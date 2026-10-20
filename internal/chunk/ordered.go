package chunk

import (
	"errors"

	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/adonm/postfgb/internal/fgb"
)

func EncodeOrdered(name string, cols []Column, features []InputFeature, bounds [][4]float64, keys []uint64, ids []string) ([]byte, []fgb.FenceEntry, error) {
	if len(features) == 0 {
		return nil, nil, errors.New("chunk: no features to encode")
	}
	if len(bounds) != len(features) || len(keys) != len(features) || len(ids) != len(features) {
		return nil, nil, errors.New("chunk: bounds, keys and ids must match the feature count")
	}
	encoded := make([][]byte, len(features))
	offsets := make([]uint64, len(features))
	total := 0
	for i := range features {
		parsed := features[i].Parsed
		if parsed == nil {
			var err error
			parsed, err = parseGeometry(&features[i].Geometry)
			if err != nil {
				return nil, nil, err
			}
		}
		builder := flatbuffers.NewBuilder(0)
		feature := writer.NewFeature(builder).
			SetGeometry(buildGeometry(builder, parsed)).
			SetProperties(encodeProperties(cols, features[i].Properties))
		offset := feature.Build()
		builder.FinishSizePrefixed(offset)
		encoded[i] = builder.FinishedBytes()
		offsets[i] = uint64(total)
		total += len(encoded[i])
	}
	envelope := [4]float64{bounds[0][0], bounds[0][1], bounds[0][2], bounds[0][3]}
	for _, b := range bounds[1:] {
		envelope[0] = min(envelope[0], b[0])
		envelope[1] = min(envelope[1], b[1])
		envelope[2] = max(envelope[2], b[2])
		envelope[3] = max(envelope[3], b[3])
	}
	headerBuilder := flatbuffers.NewBuilder(0)
	header := writer.NewHeader(headerBuilder).
		SetName(name).
		SetEnvelope(envelope[:]).
		SetGeometryType(geometryType(features)).
		SetFeaturesCount(uint64(len(features))).
		SetIndexNodeSize(16).
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
	headerOffset := header.Build()
	headerBuilder.FinishSizePrefixed(headerOffset)
	headerBytes := headerBuilder.FinishedBytes()
	items := make([]fgb.Item, len(features))
	for i := range features {
		items[i] = fgb.Item{BBox: bounds[i], Offset: offsets[i]}
	}
	index := fgb.BuildIndex(items, 16)
	featuresAt := 8 + len(headerBytes) + len(index)
	data := make([]byte, 0, featuresAt+total)
	data = append(data, writer.MagicBytes...)
	data = append(data, headerBytes...)
	data = append(data, index...)
	for _, raw := range encoded {
		data = append(data, raw...)
	}
	fence := make([]fgb.FenceEntry, 0, (len(features)+fgb.FenceEvery-1)/fgb.FenceEvery)
	for i := 0; i < len(features); i += fgb.FenceEvery {
		fence = append(fence, fgb.FenceEntry{Key: keys[i], ID: ids[i], Offset: uint64(featuresAt) + offsets[i]})
	}
	return data, fence, nil
}
