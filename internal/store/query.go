package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
)

type sqlArgs []any

func (a *sqlArgs) bind(value any) string {
	*a = append(*a, value)
	return "$" + strconv.Itoa(len(*a))
}

func bboxSQL(b *sqlArgs, prefix string, bbox *[4]float64) string {
	if bbox == nil {
		return ""
	}
	minx, miny, maxx, maxy := b.bind(bbox[0]), b.bind(bbox[1]), b.bind(bbox[2]), b.bind(bbox[3])
	return fmt.Sprintf(` and box(point(%[1]sminx, %[1]sminy), point(%[1]smaxx, %[1]smaxy)) && box(point(%[2]s::float8, %[3]s::float8), point(%[4]s::float8, %[5]s::float8)) and not (%[1]smaxx < %[2]s or %[1]sminx > %[4]s or %[1]smaxy < %[3]s or %[1]sminy > %[5]s)`,
		prefix, minx, miny, maxx, maxy)
}

func (s *Store) ChunkMetas(ctx context.Context, collection string, bbox *[4]float64, limit, tier int, after int64) ([]ChunkMeta, error) {
	var a sqlArgs
	sql := `select id, feature_count, size_bytes, columns from chunks where collection = ` + a.bind(collection) + ` and tier = ` + a.bind(tier)
	if after > 0 {
		sql += ` and id > ` + a.bind(after)
	}
	sql += bboxSQL(&a, "", bbox) + ` order by id limit ` + a.bind(limit)
	return queryAll(ctx, s.Pool, scanChunkMeta, sql, a...)
}

func scanChunkMeta(row pgx.CollectableRow) (ChunkMeta, error) {
	var meta ChunkMeta
	err := row.Scan(&meta.ID, &meta.FeatureCount, &meta.SizeBytes, &meta.Columns)
	return meta, err
}

func (s *Store) ChunkSizes(ctx context.Context, collection string, ids []int64) (map[int64]int, error) {
	rows, err := s.Pool.Query(ctx, `select id, size_bytes from chunks where collection = $1 and id = any($2)`, collection, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sizes := make(map[int64]int, len(ids))
	for rows.Next() {
		var id int64
		var size int
		if err := rows.Scan(&id, &size); err != nil {
			return nil, err
		}
		sizes[id] = size
	}
	return sizes, rows.Err()
}

func (s *Store) FindFeature(ctx context.Context, info CollectionInfo, filter cql.Expr) (FeatureMatch, bool, error) {
	ids, err := s.CandidateChunks(ctx, info, nil, filter)
	if err != nil {
		return FeatureMatch{}, false, err
	}
	for _, id := range ids {
		row, err := s.ChunkByID(ctx, info.Name, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return FeatureMatch{}, false, err
		}
		c, err := chunk.Open(row.Data)
		if err != nil {
			return FeatureMatch{}, false, err
		}
		features, err := c.SearchAll()
		if err != nil {
			return FeatureMatch{}, false, err
		}
		cols := c.Columns()
		columns := chunk.ColumnTypes(cols)
		for i, f := range features {
			if cql.Match(filter, chunk.DecodeProperties(f.PropertiesBytes(), cols), columns) {
				return FeatureMatch{ChunkID: id, Ord: i}, true, nil
			}
		}
	}
	return FeatureMatch{}, false, nil
}

func (s *Store) ChunkByID(ctx context.Context, collection string, id int64) (ChunkRow, error) {
	var row ChunkRow
	err := s.Pool.QueryRow(ctx, `select data, tier from chunks where collection = $1 and id = $2`, collection, id).
		Scan(&row.Data, &row.Tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotFound
	}
	return row, err
}

const collectionColumns = `select name, uid, kind, version, stats_version, chunk_count, feature_count, minx, miny, maxx, maxy, time_min, time_max, id_column, time_column, title, description, license, keywords, stac, members from collections`

func scanCollection(row pgx.CollectableRow) (CollectionInfo, error) {
	var info CollectionInfo
	err := row.Scan(&info.Name, &info.UID, &info.Kind, &info.Version, &info.StatsVersion, &info.ChunkCount, &info.FeatureCount,
		&info.MinX, &info.MinY, &info.MaxX, &info.MaxY, &info.TimeMin, &info.TimeMax, &info.IDColumn, &info.TimeColumn,
		&info.Title, &info.Description, &info.License, &info.Keywords, &info.Stac, &info.Members)
	return info, err
}

func (s *Store) Collections(ctx context.Context) ([]CollectionInfo, error) {
	return queryAll(ctx, s.Pool, scanCollection, collectionColumns+` order by name`)
}

func (s *Store) Collection(ctx context.Context, name string) (CollectionInfo, error) {
	return collectionInfo(ctx, s.Pool, name)
}

func collectionInfo(ctx context.Context, conn db, name string) (CollectionInfo, error) {
	rows, err := conn.Query(ctx, collectionColumns+` where name = $1`, name)
	if err != nil {
		return CollectionInfo{}, err
	}
	info, err := pgx.CollectOneRow(rows, scanCollection)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrNotFound
	}
	return info, err
}
