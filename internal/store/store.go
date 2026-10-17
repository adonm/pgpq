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
	"github.com/adonm/postfgb/internal/fgb"
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
	Data         []byte
	Size         int
	Count        int
	Tier         int
	MinX, MinY   float64
	MaxX, MaxY   float64
	Cover        string
	Checksum     string
	Columns      string
	FeatureBytes int
	HeaderBlob   []byte
	Leaves       []fgb.Leaf
}

type VirtualLayout struct {
	HeaderBytes  int
	IndexBytes   int64
	FeatureBytes int64
	Chunks       int
}

type VirtualRun struct {
	ChunkID int64
	Base    int64
	Bytes   int
}

type virtualTier struct {
	header  []byte
	leaves  [][]fgb.Leaf
	lengths []int64
	count   uint64
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

type PoolOptions struct {
	MaxConns int32
	MinConns int32
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	return OpenWithPool(ctx, dsn, PoolOptions{})
}

func OpenWithPool(ctx context.Context, dsn string, opts PoolOptions) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

func (s *Store) Ready(ctx context.Context) error {
	if err := s.Pool.Ping(ctx); err != nil {
		return err
	}
	names, err := migrationFiles()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("no migrations embedded")
	}
	var latest string
	if err := s.Pool.QueryRow(ctx,
		`select coalesce(max(version), 'none') from schema_migrations`).Scan(&latest); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return fmt.Errorf("database is not migrated: want %s", names[len(names)-1])
		}
		return err
	}
	if latest != names[len(names)-1] {
		return fmt.Errorf("database is not migrated: want %s, have %s", names[len(names)-1], latest)
	}
	return nil
}

func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	const migrationsTable = `create table if not exists schema_migrations (
		version text primary key,
		applied_at timestamptz not null default now()
	)`
	if _, err := s.Pool.Exec(ctx, migrationsTable); err != nil {
		return err
	}
	names, err := migrationFiles()
	if err != nil {
		return err
	}
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

const (
	matchOrderedBudget = 500 * time.Millisecond
	maxMatchBudget     = 4 * time.Second
)

type Ingest struct {
	tx         pgx.Tx
	collection string
	staging    map[string]string
	virtual    map[int]*virtualTier
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
	ing := &Ingest{tx: tx, collection: name, staging: map[string]string{}, virtual: map[int]*virtualTier{}}
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
		feature_bytes int not null default 0,
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
		if _, err := i.tx.Exec(ctx, `analyze `+final); err != nil {
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
	if _, err := tx.Exec(ctx, `delete from virtual_file where collection = $1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from virtual_segment where collection = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
			updated_at = now(),
			stac = excluded.stac,
			title = coalesce(excluded.title, collections.title),
			description = coalesce(excluded.description, collections.description),
			license = coalesce(excluded.license, collections.license),
			keywords = case when $6::text[] is null then collections.keywords else excluded.keywords end`,
		name, doc, meta.Title, meta.Description, meta.License, keywords)
	return err
}

func (i *Ingest) Commit(ctx context.Context) error {
	if _, err := i.tx.Exec(ctx, `set local lock_timeout = '30s'`); err != nil {
		return err
	}
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
	if err := i.buildVirtual(ctx); err != nil {
		return err
	}
	return i.tx.Commit(ctx)
}

func (i *Ingest) Rollback(ctx context.Context) {
	i.tx.Rollback(ctx)
}

func (i *Ingest) buildVirtual(ctx context.Context) error {
	var version int64
	if err := i.tx.QueryRow(ctx, `select version from collections where name = $1`, i.collection).Scan(&version); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `delete from virtual_file where collection = $1 and version <> $2`, i.collection, version); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `delete from virtual_segment where collection = $1 and version <> $2`, i.collection, version); err != nil {
		return err
	}
	for tier, chunks := range i.virtual {
		if len(chunks.leaves) == 0 {
			continue
		}
		indexBytes, envelope, ok := fgb.Index(chunks.leaves, chunks.lengths, fgb.NodeSize)
		if !ok {
			continue
		}
		header, ok := fgb.Header(i.collection, chunks.header, chunks.count, envelope)
		if !ok {
			continue
		}
		var featureBytes int64
		for _, length := range chunks.lengths {
			featureBytes += length
		}
		if _, err := i.tx.Exec(ctx, `
			insert into virtual_file (collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (collection, version, tier) do update set
				header_bytes = excluded.header_bytes, index_bytes = excluded.index_bytes,
				feature_bytes = excluded.feature_bytes, chunks = excluded.chunks`,
			i.collection, version, tier, len(header), len(indexBytes), featureBytes, len(chunks.lengths)); err != nil {
			return err
		}
		if err := i.putSegment(ctx, version, tier, 0, 0, header); err != nil {
			return err
		}
		for ordinal, at := 0, 0; at < len(indexBytes); ordinal++ {
			end := at + int(fgb.SegmentBytes)
			if end > len(indexBytes) {
				end = len(indexBytes)
			}
			if err := i.putSegment(ctx, version, tier, 1, ordinal, indexBytes[at:end]); err != nil {
				return err
			}
			at = end
		}
	}
	return nil
}

func (i *Ingest) putSegment(ctx context.Context, version int64, tier, kind, ordinal int, data []byte) error {
	_, err := i.tx.Exec(ctx, `
		insert into virtual_segment (collection, version, tier, kind, ordinal, data)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (collection, version, tier, kind, ordinal) do update set data = excluded.data`,
		i.collection, version, tier, kind, ordinal, data)
	return err
}

func (s *Store) VirtualLayout(ctx context.Context, collection string, version int64, tier int) (VirtualLayout, error) {
	var layout VirtualLayout
	err := s.Pool.QueryRow(ctx, `
		select header_bytes, index_bytes, feature_bytes, chunks from virtual_file
		where collection = $1 and version = $2 and tier = $3`, collection, version, tier).
		Scan(&layout.HeaderBytes, &layout.IndexBytes, &layout.FeatureBytes, &layout.Chunks)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, ErrNotFound
	}
	return layout, err
}

func (s *Store) VirtualSegments(ctx context.Context, collection string, version int64, tier, kind int, from, to int64) ([]byte, error) {
	first := int(from / fgb.SegmentBytes)
	last := int((to - 1) / fgb.SegmentBytes)
	rows, err := s.Pool.Query(ctx, `
		select ordinal, data from virtual_segment
		where collection = $1 and version = $2 and tier = $3 and kind = $4 and ordinal between $5 and $6
		order by ordinal`, collection, version, tier, kind, first, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]byte, 0, to-from)
	for rows.Next() {
		var ordinal int
		var data []byte
		if err := rows.Scan(&ordinal, &data); err != nil {
			return nil, err
		}
		start := int64(ordinal) * fgb.SegmentBytes
		lo := max(from-start, 0)
		hi := min(to-start, int64(len(data)))
		if lo >= hi {
			continue
		}
		out = append(out, data[lo:hi]...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != to-from {
		return nil, errors.New("virtual: segment gap")
	}
	return out, nil
}

func (s *Store) VirtualRuns(ctx context.Context, collection string, tier int) ([]VirtualRun, error) {
	rows, err := s.Pool.Query(ctx, `
		select id, feature_bytes, coalesce(sum(feature_bytes) over (order by id rows between unbounded preceding and 1 preceding), 0)
		from chunks where collection = $1 and tier = $2 order by id`, collection, tier)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []VirtualRun
	for rows.Next() {
		var run VirtualRun
		if err := rows.Scan(&run.ChunkID, &run.Bytes, &run.Base); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (i *Ingest) PutChunk(ctx context.Context, payload Payload) (int64, error) {
	id, err := putChunk(ctx, i.tx, i.staging["chunks"], i.collection, payload)
	if err != nil {
		return id, err
	}
	tier := i.virtual[payload.Tier]
	if tier == nil {
		tier = &virtualTier{header: payload.HeaderBlob}
		i.virtual[payload.Tier] = tier
	}
	tier.leaves = append(tier.leaves, payload.Leaves)
	tier.lengths = append(tier.lengths, int64(payload.FeatureBytes))
	tier.count += uint64(payload.Count)
	return id, nil
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
		update collections set kind = 'native', version = version + 1, updated_at = now(), sortable = coalesce($2::text[], '{}'),
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
		insert into `+table+` (id, collection, tier, feature_count, size_bytes, feature_bytes, minx, miny, maxx, maxy, cover, checksum, columns, data)
		values (nextval(pg_get_serial_sequence('chunks', 'id')), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10::int8multirange, $11, $12::jsonb, $13)
		returning id`,
		collection, payload.Tier, payload.Count, payload.Size, payload.FeatureBytes,
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
	budget := matchOrderedBudget + time.Duration(limit+offset)*20*time.Microsecond
	if budget > maxMatchBudget {
		budget = maxMatchBudget
	}
	probe, cancel := context.WithTimeout(ctx, budget)
	matches, err := s.matchesInIndexOrder(probe, collection, bbox, query, filter, limit, offset)
	cancel()
	if err == nil || !matchTimedOut(err) {
		return matches, err
	}
	return s.matchesByIndexScan(ctx, collection, bbox, query, filter, limit, offset)
}

func (s *Store) matchesInIndexOrder(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	from, where, args := matchesWhere(collection, bbox, query, filter)
	args = append(args, limit, offset)
	sql := fmt.Sprintf(`select m.chunk_id, m.feature_ord from %s where %s order by m.chunk_id, m.feature_ord limit $%d offset $%d`,
		from, where, len(args)-1, len(args))
	return scanMatches(ctx, s.Pool, sql, args...)
}

func (s *Store) matchesByIndexScan(ctx context.Context, collection string, bbox *[4]float64, query string, filter cql.Expr, limit, offset int) ([]FeatureMatch, error) {
	from, where, args := matchesWhere(collection, bbox, query, filter)
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

func (s *Store) FindFeature(ctx context.Context, collection string, filter cql.Expr) (FeatureMatch, bool, error) {
	from, where, args := matchesWhere(collection, nil, "", filter)
	var match FeatureMatch
	err := s.Pool.QueryRow(ctx,
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
