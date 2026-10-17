package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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
	Pool    *pgxpool.Pool
	castsMu sync.Mutex
	casts   map[string]cql.Casts
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
	Cast cql.Cast
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
	return &Store{Pool: pool, casts: map[string]cql.Casts{}}, nil
}

func (s *Store) Close() {
	s.Pool.Close()
}

func (s *Store) forgetCasts(collection string) {
	s.castsMu.Lock()
	delete(s.casts, collection)
	s.castsMu.Unlock()
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

func deleteChunksAboveTier(ctx context.Context, conn db, table string, tier int) error {
	_, err := conn.Exec(ctx, `delete from `+table+` where tier > $1`, tier)
	return err
}
