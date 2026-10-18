package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/cql"
)

func StatsReady(info CollectionInfo) bool {
	return info.Version > 0 && info.StatsVersion == info.Version
}

func (s *Store) CandidateChunks(ctx context.Context, info CollectionInfo, bbox *[4]float64, filter cql.Expr) ([]int64, error) {
	if !StatsReady(info) {
		return nil, fmt.Errorf("collection %s has no statistics for version %d (stats_version %d); re-ingest it or run postfgb backfill-stats", info.Name, info.Version, info.StatsVersion)
	}
	var a sqlArgs
	sql := `select ch.id from chunks ch where ch.collection = ` + a.bind(info.Name) + ` and ch.tier = 0`
	sql += bboxSQL(&a, "ch.", bbox)
	if filter != nil {
		sql += ` and ` + cql.ChunkSQL(filter, a.bind)
	}
	sql += ` order by ch.id`
	return queryAll(ctx, s.Pool, pgx.RowTo[int64], sql, a...)
}
