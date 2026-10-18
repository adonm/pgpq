package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/cql"
)

func StatsReady(info CollectionInfo) bool {
	return info.Version > 0 && info.StatsVersion == info.Version
}

func (s *Store) CandidateChunks(ctx context.Context, info CollectionInfo, bbox *[4]float64, filter cql.Expr) ([]int64, error) {
	casts, err := s.ColumnCasts(ctx, info)
	if err != nil {
		return nil, err
	}
	b := cql.NewBuilder("ch", casts)
	sql := `select ch.id from chunks ch where ch.collection = ` + b.Bind(info.Name) + ` and ch.tier = 0`
	sql += bboxSQL(b, "ch.", bbox)
	if filter != nil {
		sql += ` and ` + cql.ChunkSQL(filter, b)
	}
	sql += ` order by ch.id`
	return queryAll(ctx, s.Pool, pgx.RowTo[int64], sql, b.Args()...)
}
