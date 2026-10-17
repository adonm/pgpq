package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

type fgbSink struct {
	features []chunk.InputFeature
	limit    int
}

func (f *fgbSink) add(g *chunk.Feature) bool {
	if f.full() {
		return false
	}
	if g.Geometry == nil {
		return !f.full()
	}
	coords, err := json.Marshal(g.Geometry.Coordinates)
	if err != nil {
		return !f.full()
	}
	props := make(map[string]json.RawMessage, len(g.Properties))
	for name, value := range g.Properties {
		if name == "" {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		props[name] = raw
	}
	f.features = append(f.features, chunk.InputFeature{
		Geometry:   chunk.RawGeometry{Type: g.Geometry.Type, Coordinates: coords},
		Properties: props,
	})
	return !f.full()
}

func (f *fgbSink) full() bool {
	return f.limit > 0 && len(f.features) >= f.limit
}

func (f *fgbSink) remaining() int {
	if f.limit <= 0 {
		return 0
	}
	return f.limit - len(f.features)
}

func (s *Server) viewFGB(ctx context.Context, info store.CollectionInfo, q featureQuery) ([]byte, int, error) {
	members, err := s.viewMembers(ctx, info)
	if err != nil {
		return nil, 0, err
	}
	cols, err := s.unionColumns(ctx, members)
	if err != nil {
		return nil, 0, err
	}
	out := &fgbSink{limit: q.limit}
	s.streamUnion(ctx, members, func(store.CollectionInfo, viewMember) featureSink {
		return out
	}, q.bbox, q.query, q.filter, q.offset, q.overview, q.tw)
	if len(out.features) == 0 {
		return nil, 0, nil
	}
	data, err := chunk.EncodeCols(info.Name, cols, out.features)
	if err != nil {
		return nil, 0, err
	}
	return data, len(out.features), nil
}

func (s *Server) writeViewFGB(ctx context.Context, w http.ResponseWriter, r *http.Request, info store.CollectionInfo, q featureQuery) {
	data, count, err := s.viewFGB(ctx, info, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data == nil {
		w.Header().Set("Content-Type", "application/geo+json")
		fw := newFeatureWriter(w, q.limit, nil)
		fw.close("")
		return
	}
	w.Header().Set("Content-Type", "application/flatgeobuf")
	if link := pageLink(r, q.limit, q.offset, count); link != "" {
		w.Header().Set("Link", "<"+link+">; rel=\"next\"")
	}
	w.Write(data)
}

func (s *Server) unionColumns(ctx context.Context, members []unionMember) ([]chunk.Column, error) {
	types := map[string]flattypes.ColumnType{}
	for _, m := range members {
		metas, err := s.Store.ChunkMetas(ctx, m.info.Name, nil, 1, 0, 0)
		if err != nil {
			return nil, err
		}
		if len(metas) == 0 {
			continue
		}
		var raw []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(metas[0].Columns, &raw); err != nil {
			return nil, err
		}
		for _, col := range raw {
			name := col.Name
			if len(m.member.Columns) > 0 {
				mapped, ok := m.member.Columns[name]
				if !ok {
					continue
				}
				name = mapped
			}
			typ, ok := flattypes.EnumValuesColumnType[col.Type]
			if !ok {
				typ = flattypes.ColumnTypeString
			}
			if prev, seen := types[name]; seen {
				types[name] = promoteColumnType(prev, typ)
			} else {
				types[name] = typ
			}
		}
	}
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	cols := make([]chunk.Column, len(names))
	for i, name := range names {
		cols[i] = chunk.Column{Name: name, Type: types[name]}
	}
	return cols, nil
}

func promoteColumnType(a, b flattypes.ColumnType) flattypes.ColumnType {
	if a == b {
		return a
	}
	if isNumericColumn(a) && isNumericColumn(b) {
		return flattypes.ColumnTypeDouble
	}
	return flattypes.ColumnTypeString
}

func isNumericColumn(t flattypes.ColumnType) bool {
	switch t {
	case flattypes.ColumnTypeByte, flattypes.ColumnTypeUByte,
		flattypes.ColumnTypeShort, flattypes.ColumnTypeUShort,
		flattypes.ColumnTypeInt, flattypes.ColumnTypeUInt,
		flattypes.ColumnTypeLong, flattypes.ColumnTypeULong,
		flattypes.ColumnTypeFloat, flattypes.ColumnTypeDouble:
		return true
	}
	return false
}
