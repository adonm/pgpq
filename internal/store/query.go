package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/cql"
)

func bboxSQL(b *cql.Builder, prefix string, bbox *[4]float64) string {
	if bbox == nil {
		return ""
	}
	minx, miny, maxx, maxy := b.Bind(bbox[0]), b.Bind(bbox[1]), b.Bind(bbox[2]), b.Bind(bbox[3])
	return fmt.Sprintf(` and box(point(%[1]sminx, %[1]sminy), point(%[1]smaxx, %[1]smaxy)) && box(point(%[2]s::float8, %[3]s::float8), point(%[4]s::float8, %[5]s::float8)) and not (%[1]smaxx < %[2]s or %[1]sminx > %[4]s or %[1]smaxy < %[3]s or %[1]sminy > %[5]s)`,
		prefix, minx, miny, maxx, maxy)
}

func (s *Store) ChunkMetas(ctx context.Context, collection string, bbox *[4]float64, limit, tier int, after int64) ([]ChunkMeta, error) {
	b := cql.NewBuilder("", nil)
	sql := `select id, feature_count, size_bytes, columns from chunks where collection = ` + b.Bind(collection) + ` and tier = ` + b.Bind(tier)
	if after > 0 {
		sql += ` and id > ` + b.Bind(after)
	}
	sql += bboxSQL(b, "", bbox) + ` order by id limit ` + b.Bind(limit)
	return queryAll(ctx, s.Pool, scanChunkMeta, sql, b.Args()...)
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

func (s *Store) ColumnCasts(ctx context.Context, collection string) (cql.Casts, error) {
	s.castsMu.Lock()
	cached, ok := s.casts[collection]
	s.castsMu.Unlock()
	if ok {
		return cached, nil
	}
	casts := cql.Casts{}
	metas, err := s.ChunkMetas(ctx, collection, nil, 1, 0, 0)
	if err != nil || len(metas) == 0 || len(metas[0].Columns) == 0 {
		return casts, err
	}
	var cols []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if json.Unmarshal(metas[0].Columns, &cols) != nil {
		return casts, nil
	}
	for _, col := range cols {
		casts[col.Name] = CastOf(col.Type)
	}
	s.castsMu.Lock()
	s.casts[collection] = casts
	s.castsMu.Unlock()
	return casts, nil
}

func CastOf(columnType string) cql.Cast {
	switch columnType {
	case "Bool":
		return cql.CastBoolean
	case "Byte", "UByte", "Short", "UShort", "Int", "UInt", "Long", "ULong", "Float", "Double":
		return cql.CastNumeric
	case "String", "Json":
		return cql.CastText
	}
	return cql.CastNone
}

func (s *Store) matchQuery(ctx context.Context, collection string, bbox *[4]float64, filter cql.Expr) (*cql.Builder, string, error) {
	casts, err := s.ColumnCasts(ctx, collection)
	if err != nil {
		return nil, "", err
	}
	b := cql.NewBuilder("m.props", casts, collection)
	from, where := `chunk_props m`, `m.collection = $1`
	if filter != nil {
		where += ` and ` + filter.SQL(b)
	}
	if bbox != nil {
		from += ` join chunks ch on ch.collection = m.collection and ch.id = m.chunk_id`
		where += bboxSQL(b, "ch.", bbox)
	}
	return b, `from ` + from + ` where ` + where, nil
}

func (s *Store) Matches(ctx context.Context, collection string, bbox *[4]float64, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	if filter == nil {
		return nil, nil
	}
	b, from, err := s.matchQuery(ctx, collection, bbox, filter)
	if err != nil {
		return nil, err
	}
	sql := `select m.chunk_id, m.feature_ord ` + from + ` order by (m.chunk_id + 0), m.feature_ord limit ` + b.Bind(limit) + ` offset ` + b.Bind(offset)
	return queryAll(ctx, s.Pool, scanMatch, sql, b.Args()...)
}

func scanMatch(row pgx.CollectableRow) (FeatureMatch, error) {
	var match FeatureMatch
	err := row.Scan(&match.ChunkID, &match.Ord)
	return match, err
}

func (s *Store) FindFeature(ctx context.Context, collection string, filter cql.Expr) (FeatureMatch, bool, error) {
	b, from, err := s.matchQuery(ctx, collection, nil, filter)
	if err != nil {
		return FeatureMatch{}, false, err
	}
	var match FeatureMatch
	err = s.Pool.QueryRow(ctx, `select m.chunk_id, m.feature_ord `+from+` limit 1`, b.Args()...).
		Scan(&match.ChunkID, &match.Ord)
	if errors.Is(err, pgx.ErrNoRows) {
		return match, false, nil
	}
	if err != nil {
		return match, false, err
	}
	return match, true, nil
}

func (s *Store) CountMatches(ctx context.Context, collection string, filter cql.Expr) (int64, error) {
	b, from, err := s.matchQuery(ctx, collection, nil, filter)
	if err != nil {
		return 0, err
	}
	var count int64
	err = s.Pool.QueryRow(ctx, `select count(*) `+from, b.Args()...).Scan(&count)
	return count, err
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

const collectionColumns = `select name, kind, version, chunk_count, feature_count, minx, miny, maxx, maxy, time_min, time_max, id_column, time_column, title, description, license, keywords, stac, members from collections`

func scanCollection(row pgx.CollectableRow) (CollectionInfo, error) {
	var info CollectionInfo
	err := row.Scan(&info.Name, &info.Kind, &info.Version, &info.ChunkCount, &info.FeatureCount,
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
