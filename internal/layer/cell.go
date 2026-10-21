package layer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/jackc/pgx/v5/pgxpool"

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
	bytes    int64
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
	if meta.bytes == 0 {
		if err := l.pool.QueryRow(ctx, `select bytes from pgpq.cells where collection_uid = $1 and level = $2 and cell = $3`,
			coll.UID, meta.level, spatial.ToOrdered(meta.cell)).Scan(&meta.bytes); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrStale
			}
			return nil, err
		}
	}
	var reader io.ReaderAt = &cellReader{pool: l.pool, ctx: context.WithoutCancel(ctx), uid: coll.UID, level: meta.level, cell: spatial.ToOrdered(meta.cell), size: meta.bytes}
	if meta.bytes <= 1<<20 {
		var data []byte
		if err := l.pool.QueryRow(ctx, `select data from pgpq.cells where collection_uid = $1 and level = $2 and cell = $3`,
			coll.UID, meta.level, spatial.ToOrdered(meta.cell)).Scan(&data); err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	file, err := geo.Open(reader, meta.bytes)
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

type cellReader struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	uid   int64
	level int
	cell  int64
	size  int64
	mu    sync.Mutex
	cache map[int64][]byte
}

func (r *cellReader) Size() int64 {
	return r.size
}

func (r *cellReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("layer: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[int64][]byte)
	}
	written := 0
	for written < len(p) && off+int64(written) < r.size {
		at := off + int64(written)
		start := at / (64 << 10) * (64 << 10)
		block, ok := r.cache[start]
		if !ok {
			length := min(int64(64<<10), r.size-start)
			var data []byte
			err := r.pool.QueryRow(r.ctx, `select substring(data from $4 for $5) from pgpq.cells where collection_uid = $1 and level = $2 and cell = $3`,
				r.uid, r.level, r.cell, start+1, length).Scan(&data)
			if err != nil {
				return written, err
			}
			block = data
			r.cache[start] = block
		}
		lo := at - start
		if lo >= int64(len(block)) {
			return written, io.EOF
		}
		copied := copy(p[written:], block[lo:])
		written += copied
		if copied == 0 {
			break
		}
	}
	if written < len(p) {
		return written, io.EOF
	}
	return written, nil
}
