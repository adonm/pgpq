package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
	"github.com/adonm/pgpq/internal/store"
)

var (
	ErrInvalid  = errors.New("invalid upload")
	ErrConflict = errors.New("collection schema conflict")
)

const defaultMaxShardBytes int64 = 512 << 20

type Manager struct {
	pool          *pgxpool.Pool
	maxShardBytes int64
}

type Collection struct {
	Name     string
	Version  int64
	IDColumn string
	Geometry string
	Columns  []geo.Column
	Rows     int64
	Bytes    int64
}

type Result struct {
	Collection string `json:"collection"`
	Version    int64  `json:"version"`
	Features   int    `json:"features"`
	Shards     int    `json:"shards"`
	Bytes      int64  `json:"bytes"`
	ETag       string `json:"etag"`
}

type columnJSON struct {
	Name string   `json:"name"`
	Type geo.Type `json:"type"`
}

func New(pool *pgxpool.Pool) *Manager {
	limit := defaultMaxShardBytes
	if value := os.Getenv("PGPQ_SHARD_BYTES"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return &Manager{pool: pool, maxShardBytes: limit}
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
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

func sameSchema(a, b []geo.Column) bool {
	return slices.Equal(a, b)
}

func (m *Manager) List(ctx context.Context) ([]Collection, error) {
	rows, err := m.pool.Query(ctx, `select name, version, id_column, geometry, columns_json, rows, bytes from pgpq.collections order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		var columns []byte
		if err := rows.Scan(&c.Name, &c.Version, &c.IDColumn, &c.Geometry, &columns, &c.Rows, &c.Bytes); err != nil {
			return nil, err
		}
		c.Columns, err = decodeColumns(columns)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (m *Manager) Collection(ctx context.Context, name string) (Collection, error) {
	var c Collection
	var columns []byte
	err := m.pool.QueryRow(ctx, `select version, id_column, geometry, columns_json, rows, bytes from pgpq.collections where name = $1`, name).
		Scan(&c.Version, &c.IDColumn, &c.Geometry, &columns, &c.Rows, &c.Bytes)
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

func prepare(ds geo.Dataset) ([]geo.Feature, error) {
	if ds.IDColumn == "" || ds.Geometry == "" {
		return nil, invalidf("the collection needs an id and geometry column")
	}
	if len(ds.Features) == 0 {
		return nil, invalidf("the upload is empty")
	}
	features := slices.Clone(ds.Features)
	seen := make(map[string]struct{}, len(features))
	for i := range features {
		feature := &features[i]
		for _, value := range feature.Bounds {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, invalidf("feature %d has no usable geometry", i)
			}
		}
		if _, ok := seen[feature.ID]; ok {
			return nil, invalidf("duplicate id %q", feature.ID)
		}
		seen[feature.ID] = struct{}{}
		feature.Key = spatial.Key(feature.Bounds)
	}
	slices.SortFunc(features, func(a, b geo.Feature) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return features, nil
}

func featureRange(features []geo.Feature) (int64, int64) {
	return spatial.ToOrdered(features[0].Key), spatial.ToOrdered(features[len(features)-1].Key)
}

func idHash(id string) int64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	hash := uint64(offset)
	for i := 0; i < len(id); i++ {
		hash ^= uint64(id[i])
		hash *= prime
	}
	return int64(hash)
}
