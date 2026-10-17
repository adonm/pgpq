package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/adonm/postfgb/internal/cover"
	"github.com/adonm/postfgb/internal/cql"
)

func bboxPredicate(prefix string, bbox *[4]float64, next int) (string, []any) {
	if bbox == nil {
		return "", nil
	}
	minx, miny, maxx, maxy := next+1, next+2, next+3, next+4
	fragment := fmt.Sprintf(
		` and %scover && $%d::int8multirange and not (%smaxx < $%d or %sminx > $%d or %smaxy < $%d or %sminy > $%d)`,
		prefix, next, prefix, minx, prefix, maxx, prefix, miny, prefix, maxy)
	return fragment, []any{cover.Query(bbox[0], bbox[1], bbox[2], bbox[3]), bbox[0], bbox[1], bbox[2], bbox[3]}
}

func chunks(ctx context.Context, conn db, table, collection string, bbox *[4]float64, limit, tier int) ([]ChunkRow, error) {
	query := `select id, feature_count, minx, miny, maxx, maxy, data from ` + table + ` where collection = $1 and tier = $2`
	args := []any{collection, tier}
	if fragment, extra := bboxPredicate("", bbox, len(args)+1); fragment != "" {
		query += fragment
		args = append(args, extra...)
	}
	query += fmt.Sprintf(` order by id limit $%d`, len(args)+1)
	args = append(args, limit)
	return scanChunks(ctx, conn, query, args...)
}

func (s *Store) ChunkMetas(ctx context.Context, collection string, bbox *[4]float64, limit, tier int, after int64) ([]ChunkMeta, error) {
	query := `select id, feature_count, size_bytes, minx, miny, maxx, maxy, checksum, columns from chunks where collection = $1 and tier = $2`
	args := []any{collection, tier}
	if after > 0 {
		query += fmt.Sprintf(` and id > $%d`, len(args)+1)
		args = append(args, after)
	}
	if fragment, extra := bboxPredicate("", bbox, len(args)+1); fragment != "" {
		query += fragment
		args = append(args, extra...)
	}
	query += fmt.Sprintf(` order by id limit $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkMeta
	for rows.Next() {
		var meta ChunkMeta
		if err := rows.Scan(&meta.ID, &meta.FeatureCount, &meta.SizeBytes, &meta.MinX, &meta.MinY, &meta.MaxX, &meta.MaxY, &meta.Checksum, &meta.Columns); err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, rows.Err()
}

func (s *Store) ChunkSizes(ctx context.Context, collection string, ids []int64) (map[int64]int, error) {
	sizes := make(map[int64]int, len(ids))
	rows, err := s.Pool.Query(ctx,
		`select id, size_bytes from chunks where collection = $1 and id = any($2)`, collection, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (s *Store) ChunksByIDs(ctx context.Context, collection string, ids []int64) ([]ChunkRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return scanChunks(ctx, s.Pool,
		`select id, feature_count, minx, miny, maxx, maxy, data from chunks where collection = $1 and id = any($2) order by id`, collection, ids)
}

func scanChunks(ctx context.Context, conn db, query string, args ...any) ([]ChunkRow, error) {
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkRow
	for rows.Next() {
		var row ChunkRow
		if err := rows.Scan(&row.ID, &row.FeatureCount, &row.MinX, &row.MinY, &row.MaxX, &row.MaxY, &row.Data); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
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
	if err != nil {
		return casts, err
	}
	if len(metas) == 0 || len(metas[0].Columns) == 0 {
		return casts, nil
	}
	var cols []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(metas[0].Columns, &cols); err != nil {
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

func matchesWhere(collection string, casts cql.Casts, bbox *[4]float64, query string, filter cql.Expr) (string, string, []any) {
	from := `chunk_props m`
	where := `m.collection = $1`
	args := []any{collection}
	b := cql.NewBuilder("m.props", casts, collection)
	switch {
	case query != "" && filter != nil:
		from = `chunk_text m join chunk_props p on p.collection = m.collection and p.chunk_id = m.chunk_id and p.feature_ord = m.feature_ord`
		b = cql.NewBuilder("p.props", casts, collection, query)
		where += ` and m.tsv @@ websearch_to_tsquery('simple', $2) and ` + filter.SQL(b)
	case query != "":
		from = `chunk_text m`
		args = append(args, query)
		where += ` and m.tsv @@ websearch_to_tsquery('simple', $2)`
	default:
		if filter != nil {
			where += ` and ` + filter.SQL(b)
		}
	}
	args = b.Args()
	if bbox != nil {
		from += ` join chunks ch on ch.collection = m.collection and ch.id = m.chunk_id`
		fragment, extra := bboxPredicate("ch.", bbox, len(args)+1)
		where += fragment
		args = append(args, extra...)
	}
	return from, where, args
}

func (s *Store) Matches(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	if query == "" && filter == nil {
		return nil, nil
	}
	casts, err := s.ColumnCasts(ctx, collection)
	if err != nil {
		return nil, err
	}
	budget := matchOrderedBudget + time.Duration(limit+offset)*20*time.Microsecond
	if budget > maxMatchBudget {
		budget = maxMatchBudget
	}
	probe, cancel := context.WithTimeout(ctx, budget)
	matches, err := s.matchesInIndexOrder(probe, collection, casts, bbox, query, filter, limit, offset)
	cancel()
	if err == nil || !matchTimedOut(err) {
		return matches, err
	}
	return s.matchesByIndexScan(ctx, collection, casts, bbox, query, filter, limit, offset)
}

func (s *Store) matchesInIndexOrder(ctx context.Context, collection string, casts cql.Casts, bbox *[4]float64, query string, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	from, where, args := matchesWhere(collection, casts, bbox, query, filter)
	args = append(args, limit, offset)
	sql := fmt.Sprintf(`select m.chunk_id, m.feature_ord from %s where %s order by m.chunk_id, m.feature_ord limit $%d offset $%d`,
		from, where, len(args)-1, len(args))
	return scanMatches(ctx, s.Pool, sql, args...)
}

func (s *Store) matchesByIndexScan(ctx context.Context, collection string, casts cql.Casts, bbox *[4]float64, query string, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	from, where, args := matchesWhere(collection, casts, bbox, query, filter)
	args = append(args, limit, offset)
	sql := fmt.Sprintf(`select m.chunk_id, m.feature_ord from %s where %s order by (m.chunk_id + 0), m.feature_ord limit $%d offset $%d`,
		from, where, len(args)-1, len(args))
	return scanMatches(ctx, s.Pool, sql, args...)
}

func matchTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

func (s *Store) Sorted(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr, order []string, limit, offset int) ([]FeatureMatch, error) {
	casts, err := s.ColumnCasts(ctx, collection)
	if err != nil {
		return nil, err
	}
	from, where, args := matchesWhere(collection, casts, bbox, query, filter)
	args = append(args, limit, offset)
	sql := fmt.Sprintf(`select m.chunk_id, m.feature_ord from %s where %s order by %s, m.chunk_id, m.feature_ord limit $%d offset $%d`,
		from, where, strings.Join(order, ", "), len(args)-1, len(args))
	return scanMatches(ctx, s.Pool, sql, args...)
}

func scanMatches(ctx context.Context, conn db, sql string, args ...any) ([]FeatureMatch, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureMatch
	for rows.Next() {
		var match FeatureMatch
		if err := rows.Scan(&match.ChunkID, &match.Ord); err != nil {
			return nil, err
		}
		out = append(out, match)
	}
	return out, rows.Err()
}

func (s *Store) FindFeature(ctx context.Context, collection string, filter cql.Expr) (FeatureMatch, bool, error) {
	casts, err := s.ColumnCasts(ctx, collection)
	if err != nil {
		return FeatureMatch{}, false, err
	}
	from, where, args := matchesWhere(collection, casts, nil, "", filter)
	var match FeatureMatch
	err = s.Pool.QueryRow(ctx,
		fmt.Sprintf(`select m.chunk_id, m.feature_ord from %s where %s limit 1`, from, where), args...).
		Scan(&match.ChunkID, &match.Ord)
	if errors.Is(err, pgx.ErrNoRows) {
		return match, false, nil
	}
	if err != nil {
		return match, false, err
	}
	return match, true, nil
}

func (s *Store) CountMatches(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr) (int64, error) {
	casts, err := s.ColumnCasts(ctx, collection)
	if err != nil {
		return 0, err
	}
	from, where, args := matchesWhere(collection, casts, bbox, query, filter)
	var count int64
	err = s.Pool.QueryRow(ctx, fmt.Sprintf(`select count(*) from %s where %s`, from, where), args...).Scan(&count)
	return count, err
}

func (s *Store) ChunkByID(ctx context.Context, collection string, id int64) (ChunkRow, error) {
	var row ChunkRow
	err := s.Pool.QueryRow(ctx, `
		select id, feature_count, size_bytes, minx, miny, maxx, maxy, checksum, columns, data
		from chunks where collection = $1 and id = $2`, collection, id).
		Scan(&row.ID, &row.FeatureCount, &row.SizeBytes, &row.MinX, &row.MinY, &row.MaxX, &row.MaxY, &row.Checksum, &row.Columns, &row.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotFound
	}
	return row, err
}

const collectionColumns = `select name, kind, version, chunk_count, feature_count, minx, miny, maxx, maxy, time_min, time_max, id_column, time_column, sortable, title, description, license, keywords, stac from collections`

func (s *Store) Collections(ctx context.Context) ([]CollectionInfo, error) {
	rows, err := s.Pool.Query(ctx, collectionColumns+` order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectionInfo
	for rows.Next() {
		var info CollectionInfo
		if err := rows.Scan(&info.Name, &info.Kind, &info.Version, &info.ChunkCount, &info.FeatureCount,
			&info.MinX, &info.MinY, &info.MaxX, &info.MaxY, &info.TimeMin, &info.TimeMax, &info.IDColumn, &info.TimeColumn, &info.Sortable,
			&info.Title, &info.Description, &info.License, &info.Keywords, &info.Stac); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

func (s *Store) Collection(ctx context.Context, name string) (CollectionInfo, error) {
	var info CollectionInfo
	err := s.Pool.QueryRow(ctx, collectionColumns+` where name = $1`, name).
		Scan(&info.Name, &info.Kind, &info.Version, &info.ChunkCount, &info.FeatureCount,
			&info.MinX, &info.MinY, &info.MaxX, &info.MaxY, &info.TimeMin, &info.TimeMax, &info.IDColumn, &info.TimeColumn, &info.Sortable,
			&info.Title, &info.Description, &info.License, &info.Keywords, &info.Stac)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrNotFound
	}
	return info, err
}
