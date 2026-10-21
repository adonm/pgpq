package layer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adonm/pgpq/internal/cql"
	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/lru"
	"github.com/adonm/pgpq/internal/store"
)

var (
	ErrInvalid         = errors.New("invalid upload")
	ErrBadCursor       = errors.New("cursor does not match the collection")
	ErrFeatureNotFound = errors.New("feature not found")
	ErrDuplicate       = errors.New("more than one feature has this id; reload the partitions that hold it")
	ErrStale           = errors.New("the cell changed under the read")
)

const cacheBytes = 256 << 20

type Layer struct {
	pool     *pgxpool.Pool
	cells    *lru.Cache[cellKey, *geo.File]
	catalogs *lru.Cache[cellKey, []cellMeta]
}

func New(pool *pgxpool.Pool) *Layer {
	return &Layer{
		pool:     pool,
		cells:    lru.New[cellKey, *geo.File](cacheBytes, func(file *geo.File) int64 { return file.Size() }),
		catalogs: lru.New[cellKey, []cellMeta](cacheBytes, catalogSize),
	}
}

type Collection struct {
	Name     string
	UID      int64
	Version  int64
	IDColumn string
	Geometry string
	Columns  []geo.Column
}

type columnJSON struct {
	Name string   `json:"name"`
	Type cql.Type `json:"type"`
}

func encodeColumns(cols []geo.Column) ([]byte, error) {
	list := make([]columnJSON, len(cols))
	for i, col := range cols {
		list[i] = columnJSON{Name: col.Name, Type: col.Type}
	}
	return json.Marshal(list)
}

func decodeColumns(data []byte) ([]geo.Column, error) {
	var list []columnJSON
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	cols := make([]geo.Column, len(list))
	for i, col := range list {
		cols[i] = geo.Column{Name: col.Name, Type: col.Type}
	}
	return cols, nil
}

func (l *Layer) Collection(ctx context.Context, name string) (Collection, error) {
	var c Collection
	var columns []byte
	err := l.pool.QueryRow(ctx, `select uid, version, coalesce(id_column, ''), geometry, columns_json from pgpq.collections where name = $1`, name).
		Scan(&c.UID, &c.Version, &c.IDColumn, &c.Geometry, &columns)
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
