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
	MinX, MinY float64
	MaxX, MaxY float64
}

type ChunkRow struct {
	ID           int64
	FeatureCount int
	MinX, MinY   float64
	MaxX, MaxY   float64
	Data         []byte
}

type CollectionInfo struct {
	Name         string
	ChunkCount   int64
	FeatureCount int64
	MinX, MinY   *float64
	MaxX, MaxY   *float64
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

func (s *Store) PutChunks(ctx context.Context, collection string, chunks []Payload) error {
	if len(chunks) == 0 {
		return nil
	}
	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		rows[i] = []any{collection, c.Count, len(c.Data), c.MinX, c.MinY, c.MaxX, c.MaxY, c.Data}
	}
	_, err := s.Pool.CopyFrom(ctx, pgx.Identifier{"chunks"},
		[]string{"collection", "feature_count", "size_bytes", "minx", "miny", "maxx", "maxy", "data"},
		pgx.CopyFromRows(rows))
	return err
}

func (s *Store) Chunks(ctx context.Context, collection string, bbox *[4]float64, limit int) ([]ChunkRow, error) {
	query := `select id, feature_count, minx, miny, maxx, maxy, data from chunks where collection = $1`
	args := []any{collection}
	if bbox != nil {
		query += fmt.Sprintf(` and bbox && st_makeenvelope($%d, $%d, $%d, $%d, 4326)`,
			len(args)+1, len(args)+2, len(args)+3, len(args)+4)
		args = append(args, bbox[0], bbox[1], bbox[2], bbox[3])
	}
	query += fmt.Sprintf(` order by id limit $%d`, len(args)+1)
	args = append(args, limit)
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

func (s *Store) ChunkByID(ctx context.Context, collection string, id int64) ([]byte, error) {
	var data []byte
	err := s.Pool.QueryRow(ctx,
		`select data from chunks where collection = $1 and id = $2`, collection, id).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

const collectionStats = `
	select c.name,
	       count(ch.id)::bigint,
	       coalesce(sum(ch.feature_count), 0)::bigint,
	       min(ch.minx), min(ch.miny), max(ch.maxx), max(ch.maxy)
	from collections c
	left join chunks ch on ch.collection = c.name
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
