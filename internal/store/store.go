package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adonm/postfgb/internal/cover"
	"github.com/adonm/postfgb/internal/cql"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("store: not found")

var ErrConflict = errors.New("store: conflict")

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
	Size       int
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
	Kind         string
	Members      []byte
	Version      int64
	ChunkCount   int64
	FeatureCount int64
	MinX, MinY   *float64
	MaxX, MaxY   *float64
	TimeMin      *time.Time
	TimeMax      *time.Time
	IDColumn     *string
	TimeColumn   *string
	Sortable     []string
	Title        *string
	Description  *string
	License      *string
	Keywords     []string
	Stac         []byte
}

type CollectionMeta struct {
	Title       *string
	Description *string
	License     *string
	Keywords    *[]string
}

type FeatureMatch struct {
	ChunkID int64
	Ord     int
}

type SortColumn struct {
	Name string
	Cast string
}

type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
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

var collectionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,47}$`)

func ValidName(name string) bool {
	return collectionName.MatchString(name)
}

var chunkPartitions = []string{"chunks", "chunk_text", "chunk_props"}

type Ingest struct {
	tx         pgx.Tx
	collection string
	staging    map[string]string
}

func (s *Store) BeginIngest(ctx context.Context, name string) (*Ingest, error) {
	if !collectionName.MatchString(name) {
		return nil, fmt.Errorf("invalid collection name %q", name)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`insert into collections (name) values ($1) on conflict (name) do nothing`, name); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	ing := &Ingest{tx: tx, collection: name, staging: map[string]string{}}
	if err := ing.createStaging(ctx); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return ing, nil
}

func (i *Ingest) createStaging(ctx context.Context) error {
	for _, parent := range chunkPartitions {
		i.staging[parent] = parent + "_" + i.collection + "_next"
	}
	chunks := pgx.Identifier{i.staging["chunks"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create table `+chunks+` (
		id bigint not null,
		collection text not null,
		tier smallint not null default 0,
		feature_count int not null,
		size_bytes int not null,
		minx double precision not null,
		miny double precision not null,
		maxx double precision not null,
		maxy double precision not null,
		cover int8multirange not null,
		checksum text,
		columns jsonb not null default '[]',
		data bytea not null,
		primary key (collection, id)
	)`); err != nil {
		return err
	}
	text := pgx.Identifier{i.staging["chunk_text"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create table `+text+` (
		collection text not null,
		chunk_id bigint not null,
		feature_ord int not null,
		tsv tsvector not null,
		primary key (collection, chunk_id, feature_ord)
	)`); err != nil {
		return err
	}
	props := pgx.Identifier{i.staging["chunk_props"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create table `+props+` (
		collection text not null,
		chunk_id bigint not null,
		feature_ord int not null,
		props jsonb not null,
		primary key (collection, chunk_id, feature_ord)
	)`); err != nil {
		return err
	}
	return nil
}

func (i *Ingest) createIndexes(ctx context.Context) error {
	chunks := pgx.Identifier{i.staging["chunks"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create index on `+chunks+` using gist (cover)`); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `create index on `+chunks+` (tier, id)`); err != nil {
		return err
	}
	text := pgx.Identifier{i.staging["chunk_text"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create index on `+text+` using gin (tsv)`); err != nil {
		return err
	}
	props := pgx.Identifier{i.staging["chunk_props"]}.Sanitize()
	_, err := i.tx.Exec(ctx, `create index on `+props+` using gin (props jsonb_path_ops)`)
	return err
}

func (i *Ingest) swap(ctx context.Context) error {
	view := pgx.Identifier{"layers", i.collection}.Sanitize()
	if _, err := i.tx.Exec(ctx, `drop view if exists `+view); err != nil {
		return err
	}
	for j := len(chunkPartitions) - 1; j >= 0; j-- {
		old := pgx.Identifier{chunkPartitions[j] + "_" + i.collection}.Sanitize()
		if _, err := i.tx.Exec(ctx, `drop table if exists `+old); err != nil {
			return err
		}
	}
	for _, parent := range chunkPartitions {
		staging := pgx.Identifier{i.staging[parent]}.Sanitize()
		final := pgx.Identifier{parent + "_" + i.collection}.Sanitize()
		if _, err := i.tx.Exec(ctx, `alter table `+staging+` rename to `+final); err != nil {
			return err
		}
		if _, err := i.tx.Exec(ctx, `alter table `+parent+` attach partition `+final+` for values in ('`+i.collection+`')`); err != nil {
			return err
		}
		if err := i.renameIndexes(ctx, parent+"_"+i.collection); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingest) renameIndexes(ctx context.Context, table string) error {
	rows, err := i.tx.Query(ctx, `select indexname from pg_indexes where tablename = $1`, table)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		renamed := strings.Replace(name, "_next_", "_", 1)
		if renamed == name {
			continue
		}
		if _, err := i.tx.Exec(ctx,
			`alter index `+pgx.Identifier{name}.Sanitize()+` rename to `+pgx.Identifier{renamed}.Sanitize()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DropCollection(ctx context.Context, name string) error {
	if !collectionName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx,
		`select exists (select 1 from collections where name = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	view := pgx.Identifier{"layers", name}.Sanitize()
	if _, err := tx.Exec(ctx, `drop view if exists `+view); err != nil {
		return err
	}
	for j := len(chunkPartitions) - 1; j >= 0; j-- {
		partition := pgx.Identifier{chunkPartitions[j] + "_" + name}.Sanitize()
		if _, err := tx.Exec(ctx, `drop table if exists `+partition); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `delete from collections where name = $1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from tile_cache where collection = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PutView(ctx context.Context, name string, members []byte, meta CollectionMeta) error {
	if !collectionName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	tag, err := s.Pool.Exec(ctx, `
		insert into collections (name, kind, members) values ($1, 'view', $2::jsonb)
		on conflict (name) do update set kind = 'view', members = excluded.members, version = collections.version + 1
		where collections.kind = 'view'`, name, members)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	var set []string
	args := []any{name}
	if meta.Title != nil {
		args = append(args, nullable(*meta.Title))
		set = append(set, fmt.Sprintf("title = $%d", len(args)))
	}
	if meta.Description != nil {
		args = append(args, nullable(*meta.Description))
		set = append(set, fmt.Sprintf("description = $%d", len(args)))
	}
	if meta.License != nil {
		args = append(args, nullable(*meta.License))
		set = append(set, fmt.Sprintf("license = $%d", len(args)))
	}
	if meta.Keywords != nil {
		args = append(args, *meta.Keywords)
		set = append(set, fmt.Sprintf("keywords = coalesce($%d::text[], '{}')", len(args)))
	}
	if len(set) == 0 {
		return nil
	}
	_, err = s.Pool.Exec(ctx, `update collections set `+strings.Join(set, ", ")+` where name = $1`, args...)
	return err
}

func (s *Store) PutMeta(ctx context.Context, name string, doc []byte, meta CollectionMeta) error {
	if !collectionName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	keywords := any(nil)
	if meta.Keywords != nil {
		keywords = *meta.Keywords
	}
	_, err := s.Pool.Exec(ctx, `
		insert into collections (name, stac, title, description, license, keywords)
		values ($1, $2::jsonb, $3, $4, $5, coalesce($6::text[], '{}'))
		on conflict (name) do update set
			stac = excluded.stac,
			title = coalesce(excluded.title, collections.title),
			description = coalesce(excluded.description, collections.description),
			license = coalesce(excluded.license, collections.license),
			keywords = case when $6::text[] is null then collections.keywords else excluded.keywords end`,
		name, doc, meta.Title, meta.Description, meta.License, keywords)
	return err
}

func (s *Store) TileCacheGet(ctx context.Context, collection, version string, tier, z, x, y int, filterKey string) ([]byte, string, bool, error) {
	var data []byte
	var etag string
	err := s.Pool.QueryRow(ctx, `
		select data, etag from tile_cache
		where collection = $1 and version = $2 and tier = $3 and z = $4 and x = $5 and y = $6 and filter_key = $7`,
		collection, version, tier, z, x, y, filterKey).Scan(&data, &etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return data, etag, true, nil
}

func (s *Store) TileCachePut(ctx context.Context, collection, version string, tier, z, x, y int, filterKey, etag string, data []byte) error {
	_, err := s.Pool.Exec(ctx, `
		insert into tile_cache (collection, version, tier, z, x, y, filter_key, etag, data)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict do nothing`,
		collection, version, tier, z, x, y, filterKey, etag, data)
	return err
}

func (s *Store) PurgeTileCache(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `delete from tile_cache where created_at < now() - interval '6 hours'`)
	return err
}

func (i *Ingest) Commit(ctx context.Context) error {
	if err := i.createIndexes(ctx); err != nil {
		return err
	}
	if err := i.swap(ctx); err != nil {
		return err
	}
	if err := refreshStats(ctx, i.tx, i.collection); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `create schema if not exists layers`); err != nil {
		return err
	}
	view := pgx.Identifier{"layers", i.collection}.Sanitize()
	if _, err := i.tx.Exec(ctx,
		`create or replace view `+view+` as select * from chunks where collection = '`+i.collection+`' and tier = 0`); err != nil {
		return err
	}
	return i.tx.Commit(ctx)
}

func (i *Ingest) Rollback(ctx context.Context) {
	i.tx.Rollback(ctx)
}

func (i *Ingest) PutChunk(ctx context.Context, payload Payload) (int64, error) {
	return putChunk(ctx, i.tx, i.staging["chunks"], i.collection, payload)
}

func (i *Ingest) PutChunkText(ctx context.Context, chunkID int64, docs []string) error {
	return putChunkText(ctx, i.tx, i.staging["chunk_text"], i.collection, chunkID, docs)
}

func (i *Ingest) PutChunkProps(ctx context.Context, chunkID int64, docs []string) error {
	return putChunkProps(ctx, i.tx, i.staging["chunk_props"], i.collection, chunkID, docs)
}

func (i *Ingest) Chunks(ctx context.Context, bbox *[4]float64, limit, tier int) ([]ChunkRow, error) {
	return chunks(ctx, i.tx, i.staging["chunks"], i.collection, bbox, limit, tier)
}

func (i *Ingest) DeleteChunksAboveTier(ctx context.Context, tier int) error {
	return deleteChunksAboveTier(ctx, i.tx, i.staging["chunks"], tier)
}

func (i *Ingest) SetCatalog(ctx context.Context, sortable []string, idColumn, timeColumn string, timeMin, timeMax *time.Time) error {
	_, err := i.tx.Exec(ctx, `
		update collections set kind = 'native', version = version + 1, sortable = coalesce($2::text[], '{}'),
			id_column = $3, time_column = $4, time_min = $5, time_max = $6
		where name = $1`, i.collection, sortable, nullable(idColumn), nullable(timeColumn), timeMin, timeMax)
	return err
}

func SortExpr(name, cast string) string {
	key := strings.ReplaceAll(name, "'", "''")
	expr := `props->>'` + key + `'`
	switch cast {
	case "":
		return expr
	case "numeric", "boolean", "timestamptz":
		return `(` + expr + `)::` + cast
	default:
		return ""
	}
}

func (i *Ingest) CreateSortIndexes(ctx context.Context, cols []SortColumn) error {
	for idx, col := range cols {
		expr := SortExpr(col.Name, col.Cast)
		if expr == "" {
			return fmt.Errorf("sortable: unsupported cast %q", col.Cast)
		}
		name := pgx.Identifier{fmt.Sprintf("chunk_props_%s_sort_%d_idx", i.collection, idx)}.Sanitize()
		table := pgx.Identifier{i.staging["chunk_props"]}.Sanitize()
		if _, err := i.tx.Exec(ctx, `create index `+name+` on `+table+` ((`+expr+`))`); err != nil {
			return err
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func putChunk(ctx context.Context, conn db, table, collection string, payload Payload) (int64, error) {
	var id int64
	err := conn.QueryRow(ctx, `
		insert into `+table+` (id, collection, tier, feature_count, size_bytes, minx, miny, maxx, maxy, cover, checksum, columns, data)
		values (nextval(pg_get_serial_sequence('chunks', 'id')), $1, $2, $3, $4, $5, $6, $7, $8, $9::int8multirange, $10, $11::jsonb, $12)
		returning id`,
		collection, payload.Tier, payload.Count, payload.Size,
		payload.MinX, payload.MinY, payload.MaxX, payload.MaxY, payload.Cover,
		payload.Checksum, payload.Columns, payload.Data).Scan(&id)
	return id, err
}

func putChunkText(ctx context.Context, conn db, table, collection string, chunkID int64, docs []string) error {
	if len(docs) == 0 {
		return nil
	}
	ords := make([]int32, len(docs))
	for i := range ords {
		ords[i] = int32(i)
	}
	_, err := conn.Exec(ctx, `
		insert into `+table+` (collection, chunk_id, feature_ord, tsv)
		select $1, $2, ord, to_tsvector('simple', doc)
		from unnest($3::int[], $4::text[]) as input(ord, doc)
		where doc <> ''`, collection, chunkID, ords, docs)
	return err
}

func putChunkProps(ctx context.Context, conn db, table, collection string, chunkID int64, docs []string) error {
	if len(docs) == 0 {
		return nil
	}
	ords := make([]int32, len(docs))
	for i := range ords {
		ords[i] = int32(i)
	}
	_, err := conn.Exec(ctx, `
		insert into `+table+` (collection, chunk_id, feature_ord, props)
		select $1, $2, ord, doc::jsonb
		from unnest($3::int[], $4::text[]) as input(ord, doc)
		where doc <> '' and doc <> '{}' and doc <> 'null'`, collection, chunkID, ords, docs)
	return err
}

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

func deleteChunksAboveTier(ctx context.Context, conn db, table string, tier int) error {
	_, err := conn.Exec(ctx, `delete from `+table+` where tier > $1`, tier)
	return err
}

func refreshStats(ctx context.Context, conn db, name string) error {
	_, err := conn.Exec(ctx, `
		update collections set
			chunk_count = s.chunks,
			feature_count = s.features,
			minx = s.minx, miny = s.miny, maxx = s.maxx, maxy = s.maxy
		from (
			select count(*) chunks, coalesce(sum(feature_count), 0) features,
			       min(minx) minx, min(miny) miny, max(maxx) maxx, max(maxy) maxy
			from chunks where collection = $1 and tier = 0
		) s
		where collections.name = $1`, name)
	return err
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

func (s *Store) Chunks(ctx context.Context, collection string, bbox *[4]float64, limit, tier int) ([]ChunkRow, error) {
	return chunks(ctx, s.Pool, "chunks", collection, bbox, limit, tier)
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

func matchesWhere(collection string, bbox *[4]float64, query string, filter cql.Expr) (string, string, []any) {
	from := `chunk_props m`
	where := `m.collection = $1`
	args := []any{collection}
	switch {
	case query != "" && filter != nil:
		from = `chunk_text m join chunk_props p on p.collection = m.collection and p.chunk_id = m.chunk_id and p.feature_ord = m.feature_ord`
		args = append(args, query)
		where += ` and m.tsv @@ websearch_to_tsquery('simple', $2) and ` + filter.SQL("p.props", &args)
	case query != "":
		from = `chunk_text m`
		args = append(args, query)
		where += ` and m.tsv @@ websearch_to_tsquery('simple', $2)`
	default:
		if filter != nil {
			where += ` and ` + filter.SQL("m.props", &args)
		}
	}
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
	from, where, args := matchesWhere(collection, bbox, query, filter)
	args = append(args, limit, offset)
	sql := fmt.Sprintf(`select m.chunk_id, m.feature_ord from %s where %s order by m.chunk_id, m.feature_ord limit $%d offset $%d`,
		from, where, len(args)-1, len(args))
	return scanMatches(ctx, s.Pool, sql, args...)
}

func (s *Store) Sorted(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr, order []string, limit, offset int) ([]FeatureMatch, error) {
	from, where, args := matchesWhere(collection, bbox, query, filter)
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

func (s *Store) CountMatches(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr) (int64, error) {
	from, where, args := matchesWhere(collection, bbox, query, filter)
	var count int64
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(`select count(*) from %s where %s`, from, where), args...).Scan(&count)
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

const collectionColumns = `select name, kind, members, version, chunk_count, feature_count, minx, miny, maxx, maxy, time_min, time_max, id_column, time_column, sortable, title, description, license, keywords, stac from collections`

func (s *Store) Collections(ctx context.Context) ([]CollectionInfo, error) {
	rows, err := s.Pool.Query(ctx, collectionColumns+` order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectionInfo
	for rows.Next() {
		var info CollectionInfo
		if err := rows.Scan(&info.Name, &info.Kind, &info.Members, &info.Version, &info.ChunkCount, &info.FeatureCount,
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
		Scan(&info.Name, &info.Kind, &info.Members, &info.Version, &info.ChunkCount, &info.FeatureCount,
			&info.MinX, &info.MinY, &info.MaxX, &info.MaxY, &info.TimeMin, &info.TimeMax, &info.IDColumn, &info.TimeColumn, &info.Sortable,
			&info.Title, &info.Description, &info.License, &info.Keywords, &info.Stac)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrNotFound
	}
	return info, err
}
