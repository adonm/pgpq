package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type AttrRow struct {
	ID           int64
	FeatureBase  int64
	FeatureCount int
	Data         []byte
}

func (s *Store) AttrReady(ctx context.Context, collection string) (bool, error) {
	var ready bool
	err := s.Pool.QueryRow(ctx, `select exists(select 1 from attr_chunks where collection = $1)`, collection).Scan(&ready)
	return ready, err
}

func (s *Store) AttrRows(ctx context.Context, collection string, from, to int64) ([]AttrRow, error) {
	return queryAll(ctx, s.Pool, func(row pgx.CollectableRow) (AttrRow, error) {
		var r AttrRow
		err := row.Scan(&r.ID, &r.FeatureBase, &r.FeatureCount, &r.Data)
		return r, err
	}, `select id, feature_base, feature_count, data from attr_chunks where collection = $1 and feature_base < $3 and feature_base + feature_count > $2 order by feature_base`,
		collection, from, to)
}

type ChunkBase struct {
	Base  int64
	Count int
}

func (s *Store) ChunkBases(ctx context.Context, collection string, ids []int64) (map[int64]ChunkBase, error) {
	rows, err := s.Pool.Query(ctx, `select id, feature_base, feature_count from chunks where collection = $1 and id = any($2)`, collection, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]ChunkBase, len(ids))
	for rows.Next() {
		var id int64
		var base ChunkBase
		if err := rows.Scan(&id, &base.Base, &base.Count); err != nil {
			return nil, err
		}
		out[id] = base
	}
	return out, rows.Err()
}
