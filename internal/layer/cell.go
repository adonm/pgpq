package layer

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
)

type cellKey struct {
	uid     int64
	version int64
	level   int
	cell    uint64
}

type cellMeta struct {
	cell     uint64
	level    int
	features int
	bounds   [4]float64
	firstKey uint64
	firstID  string
	lastKey  uint64
	lastID   string
	zone     string
}

type catalogKey struct {
	uid     int64
	version int64
	level   int
	cell    uint64
}

func (l *Layer) cellFile(ctx context.Context, coll Collection, meta cellMeta) (*geo.File, error) {
	key := cellKey{uid: coll.UID, version: coll.Version, level: meta.level, cell: meta.cell}
	if file, ok := l.cells.Get(key); ok {
		return file, nil
	}
	var data []byte
	err := l.pool.QueryRow(ctx, `select data from pgpq.cells where collection_uid = $1 and level = $2 and cell = $3`,
		coll.UID, meta.level, spatial.ToOrdered(meta.cell)).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStale
	}
	if err != nil {
		return nil, err
	}
	file, err := geo.Open(data)
	if err != nil {
		return nil, err
	}
	l.cells.Add(key, file)
	return file, nil
}

func catalogSize(metas []cellMeta) int64 {
	var n int64
	for _, meta := range metas {
		n += int64(192 + len(meta.zone) + len(meta.lastID))
	}
	return n
}
