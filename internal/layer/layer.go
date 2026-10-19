package layer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adonm/postfgb/internal/blocks"
	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/lru"
	"github.com/adonm/postfgb/internal/store"
)

var (
	ErrInvalid         = ingest.ErrInvalid
	ErrGone            = errors.New("snapshot expired")
	ErrBadCursor       = errors.New("cursor does not match the collection")
	ErrFeatureNotFound = errors.New("feature not found")
	ErrDuplicate       = errors.New("more than one feature has this id; reload the partitions that hold it")
)

const Retention = 72 * time.Hour

const cacheBytes = 256 << 20

type Layer struct {
	pool     *pgxpool.Pool
	blocks   *blocks.Store
	runs     *lru.Cache[int64, *idRun]
	catalogs *lru.Cache[catalogKey, []chunkMeta]
}

func New(pool *pgxpool.Pool) *Layer {
	return &Layer{
		pool:     pool,
		blocks:   blocks.New(pool, cacheBytes),
		runs:     lru.New[int64, *idRun](cacheBytes, runSize),
		catalogs: lru.New[catalogKey, []chunkMeta](cacheBytes, catalogSize),
	}
}

type Collection struct {
	Name     string
	UID      int64
	Snapshot int64
	IDColumn string
	Columns  []chunk.Column
}

type columnJSON struct {
	Name string `json:"name"`
	Type int    `json:"type"`
}

func encodeColumns(cols []chunk.Column) ([]byte, error) {
	list := make([]columnJSON, len(cols))
	for i, col := range cols {
		list[i] = columnJSON{Name: col.Name, Type: int(col.Type)}
	}
	return json.Marshal(list)
}

func decodeColumns(data []byte) ([]chunk.Column, error) {
	var list []columnJSON
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	cols := make([]chunk.Column, len(list))
	for i, col := range list {
		cols[i] = chunk.Column{Name: col.Name, Type: flattypes.ColumnType(col.Type)}
	}
	return cols, nil
}

func (l *Layer) Collection(ctx context.Context, name string) (Collection, error) {
	var c Collection
	var columns []byte
	err := l.pool.QueryRow(ctx, `select uid, current_snapshot, coalesce(id_column, ''), columns_json from pfgb.collections where name = $1`, name).
		Scan(&c.UID, &c.Snapshot, &c.IDColumn, &columns)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, store.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Name = name
	c.Columns, err = decodeColumns(columns)
	return c, err
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
