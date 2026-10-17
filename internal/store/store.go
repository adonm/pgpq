package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adonm/postfgb/internal/cover"
	"github.com/adonm/postfgb/internal/cql"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("store: not found")

var TrackedExtensions = []string{
	"postgis",
	"pg_trgm",
	"pg_bigm",
	"unaccent",
	"btree_gin",
	"btree_gist",
	"bloom",
	"hll",
	"pg_roaringbitmap",
	"pgvector",
	"h3-pg",
}

type Store struct {
	Pool *pgxpool.Pool
}

type Payload struct {
	Data       []byte
	Count      int
	Tier       int
	MinX, MinY float64
	MaxX, MaxY float64
	Cover      string
	Checksum   string
	Columns    string
}

type ChunkRow struct {
	ID           int64
	FeatureCount int
	SizeBytes    int
	MinX, MinY   float64
	MaxX, MaxY   float64
	Checksum     *string
	Columns      []byte
	Data         []byte
}

type ChunkMeta struct {
	ID           int64
	FeatureCount int
	SizeBytes    int
	MinX, MinY   float64
	MaxX, MaxY   float64
	Checksum     *string
	Columns      []byte
}

type CollectionInfo struct {
	Name         string
	ChunkCount   int64
	FeatureCount int64
	MinX, MinY   *float64
	MaxX, MaxY   *float64
}

type FeatureMatch struct {
	ChunkID int64
	Ord     int
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() {
	s.Pool.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	const migrationsTable = `create table if not exists schema_migrations (
		version text primary key,
		applied_at timestamptz not null default now()
	)`
	if _, err := s.Pool.Exec(ctx, migrationsTable); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var applied bool
		if err := s.Pool.QueryRow(ctx,
			`select exists (select 1 from schema_migrations where version = $1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := s.Pool.Exec(ctx, `insert into schema_migrations (version) values ($1)`, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Capabilities(ctx context.Context) (map[string]bool, error) {
	caps := make(map[string]bool, len(TrackedExtensions))
	for _, name := range TrackedExtensions {
		caps[name] = false
	}
	rows, err := s.Pool.Query(ctx,
		`select name from pg_available_extensions where name = any($1)`, TrackedExtensions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		caps[name] = true
	}
	return caps, rows.Err()
}

func (s *Store) EnsureCollection(ctx context.Context, name string) error {
	_, err := s.Pool.Exec(ctx,
		`insert into collections (name) values ($1) on conflict (name) do nothing`, name)
	return err
}

func (s *Store) PutChunk(ctx context.Context, collection string, payload Payload) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		insert into chunks (collection, tier, feature_count, size_bytes, minx, miny, maxx, maxy, cover, checksum, columns, data)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9::int8multirange, $10, $11::jsonb, $12)
		returning id`,
		collection, payload.Tier, payload.Count, len(payload.Data),
		payload.MinX, payload.MinY, payload.MaxX, payload.MaxY, payload.Cover,
		payload.Checksum, payload.Columns, payload.Data).Scan(&id)
	return id, err
}

func (s *Store) PutChunkText(ctx context.Context, chunkID int64, docs []string) error {
	if len(docs) == 0 {
		return nil
	}
	ords := make([]int32, len(docs))
	for i := range ords {
		ords[i] = int32(i)
	}
	_, err := s.Pool.Exec(ctx, `
		insert into chunk_text (chunk_id, feature_ord, tsv)
		select $1, ord, to_tsvector('simple', doc)
		from unnest($2::int[], $3::text[]) as input(ord, doc)
		where doc <> ''`, chunkID, ords, docs)
	return err
}

func (s *Store) PutChunkProps(ctx context.Context, chunkID int64, docs []string) error {
	if len(docs) == 0 {
		return nil
	}
	ords := make([]int32, len(docs))
	for i := range ords {
		ords[i] = int32(i)
	}
	_, err := s.Pool.Exec(ctx, `
		insert into chunk_props (chunk_id, feature_ord, props)
		select $1, ord, doc::jsonb
		from unnest($2::int[], $3::text[]) as input(ord, doc)
		where doc <> '' and doc <> '{}' and doc <> 'null'`, chunkID, ords, docs)
	return err
}

func bboxPredicate(bbox *[4]float64, next int) (string, []any) {
	if bbox == nil {
		return "", nil
	}
	minx, miny, maxx, maxy := next+1, next+2, next+3, next+4
	fragment := fmt.Sprintf(
		` and cover && $%d::int8multirange and not (maxx < $%d or minx > $%d or maxy < $%d or miny > $%d)`,
		next, minx, maxx, miny, maxy)
	return fragment, []any{cover.Query(bbox[0], bbox[1], bbox[2], bbox[3]), bbox[0], bbox[1], bbox[2], bbox[3]}
}

func (s *Store) DeleteChunksAboveTier(ctx context.Context, collection string, tier int) error {
	_, err := s.Pool.Exec(ctx, `delete from chunks where collection = $1 and tier > $2`, collection, tier)
	return err
}

func (s *Store) Chunks(ctx context.Context, collection string, bbox *[4]float64, limit, tier int) ([]ChunkRow, error) {
	query := `select id, feature_count, minx, miny, maxx, maxy, data from chunks where collection = $1 and tier = $2`
	args := []any{collection, tier}
	if fragment, extra := bboxPredicate(bbox, len(args)+1); fragment != "" {
		query += fragment
		args = append(args, extra...)
	}
	query += fmt.Sprintf(` order by id limit $%d`, len(args)+1)
	args = append(args, limit)
	return s.scanChunks(ctx, query, args...)
}

func (s *Store) ChunkMetas(ctx context.Context, collection string, bbox *[4]float64, limit, tier int) ([]ChunkMeta, error) {
	query := `select id, feature_count, size_bytes, minx, miny, maxx, maxy, checksum, columns from chunks where collection = $1 and tier = $2`
	args := []any{collection, tier}
	if fragment, extra := bboxPredicate(bbox, len(args)+1); fragment != "" {
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

func (s *Store) ChunksByIDs(ctx context.Context, ids []int64) ([]ChunkRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.scanChunks(ctx,
		`select id, feature_count, minx, miny, maxx, maxy, data from chunks where id = any($1) order by id`, ids)
}

func (s *Store) scanChunks(ctx context.Context, query string, args ...any) ([]ChunkRow, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
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

func (s *Store) Matches(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	if query == "" && filter == nil {
		return nil, nil
	}
	sql := `select m.chunk_id, m.feature_ord from `
	args := []any{collection}
	switch {
	case query != "" && filter != nil:
		args = append(args, query)
		where := filter.SQL("p.props", &args)
		sql += fmt.Sprintf(`chunk_text m
			join chunk_props p on p.chunk_id = m.chunk_id and p.feature_ord = m.feature_ord
			join chunks ch on ch.id = m.chunk_id
			where ch.collection = $1 and m.tsv @@ websearch_to_tsquery('simple', $2) and %s`, where)
	case query != "":
		args = append(args, query)
		sql += `chunk_text m
			join chunks ch on ch.id = m.chunk_id
			where ch.collection = $1 and m.tsv @@ websearch_to_tsquery('simple', $2)`
	default:
		where := filter.SQL("m.props", &args)
		sql += fmt.Sprintf(`chunk_props m
			join chunks ch on ch.id = m.chunk_id
			where ch.collection = $1 and %s`, where)
	}
	if fragment, extra := bboxPredicate(bbox, len(args)+1); fragment != "" {
		sql += fragment
		args = append(args, extra...)
	}
	sql += fmt.Sprintf(` order by m.chunk_id, m.feature_ord limit $%d offset $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)
	rows, err := s.Pool.Query(ctx, sql, args...)
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

const collectionStats = `
	select c.name,
	       count(ch.id)::bigint,
	       coalesce(sum(ch.feature_count), 0)::bigint,
	       min(ch.minx), min(ch.miny), max(ch.maxx), max(ch.maxy)
	from collections c
	left join chunks ch on ch.collection = c.name and ch.tier = 0
	%s
	group by c.name
	order by c.name`

func (s *Store) Collections(ctx context.Context) ([]CollectionInfo, error) {
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(collectionStats, ""))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectionInfo
	for rows.Next() {
		var info CollectionInfo
		if err := rows.Scan(&info.Name, &info.ChunkCount, &info.FeatureCount,
			&info.MinX, &info.MinY, &info.MaxX, &info.MaxY); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

func (s *Store) Collection(ctx context.Context, name string) (CollectionInfo, error) {
	var info CollectionInfo
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(collectionStats, "where c.name = $1"), name).
		Scan(&info.Name, &info.ChunkCount, &info.FeatureCount,
			&info.MinX, &info.MinY, &info.MaxX, &info.MaxY)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrNotFound
	}
	return info, err
}
