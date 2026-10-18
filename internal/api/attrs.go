package api

import (
	"context"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

func (s *Server) attrOrds(ctx context.Context, target store.CollectionInfo, base int64, count int, filter cql.Expr) ([]int, bool, error) {
	if count == 0 {
		return nil, true, nil
	}
	rows, err := s.Store.AttrRows(ctx, target.Name, base, base+int64(count))
	if err != nil {
		return nil, false, err
	}
	covered := make([]bool, count)
	for _, row := range rows {
		lo := int(row.FeatureBase - base)
		if lo < 0 {
			lo = 0
		}
		hi := int(row.FeatureBase + int64(row.FeatureCount) - base)
		if hi > count {
			hi = count
		}
		for i := lo; i < hi; i++ {
			covered[i] = true
		}
	}
	for _, ok := range covered {
		if !ok {
			return nil, false, nil
		}
	}
	props := make([]map[string]any, count)
	names := cql.Properties(filter)
	var columns map[string]flattypes.ColumnType
	for _, row := range rows {
		file, ok := chunk.ReadAttrHeader(row.Data)
		if !ok {
			return nil, false, nil
		}
		if columns == nil {
			columns = file.ColumnTypes()
		}
		for _, name := range names {
			if _, ok := file.Column(name); !ok {
				continue
			}
			values, err := chunk.DecodeAttrColumn(row.Data, name)
			if err != nil {
				return nil, false, err
			}
			for i, value := range values {
				if value == nil {
					continue
				}
				local := int(row.FeatureBase) + i - int(base)
				if local < 0 || local >= count {
					continue
				}
				if props[local] == nil {
					props[local] = map[string]any{}
				}
				props[local][name] = value
			}
		}
	}
	ords := make([]int, 0, count)
	for i, p := range props {
		if cql.Match(filter, p, columns) {
			ords = append(ords, i)
		}
	}
	return ords, true, nil
}

func (s *Server) attrFilter(ctx context.Context, target store.CollectionInfo, ids []int64, filter cql.Expr) (map[int64][]int, bool, error) {
	bases, err := s.Store.ChunkBases(ctx, target.Name, ids)
	if err != nil {
		return nil, false, err
	}
	out := make(map[int64][]int, len(ids))
	for _, id := range ids {
		base, ok := bases[id]
		if !ok {
			return nil, false, nil
		}
		ords, ok, err := s.attrOrds(ctx, target, base.Base, base.Count, filter)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		out[id] = ords
	}
	return out, true, nil
}
