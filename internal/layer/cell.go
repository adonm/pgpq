package layer

import (
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
	cellID   int64
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
	if meta.bytes == 0 || meta.cellID == 0 {
		if err := l.pool.QueryRow(ctx, `select bytes, cell_id from pgpq.cells where collection_uid = $1 and level = $2 and cell = $3`,
			coll.UID, meta.level, spatial.ToOrdered(meta.cell)).Scan(&meta.bytes, &meta.cellID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrStale
			}
			return nil, err
		}
	}
	reader := &chunkReader{pool: l.pool, ctx: context.WithoutCancel(ctx), cellID: meta.cellID, size: meta.bytes, cache: make(map[int32][]byte)}
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

type chunkReader struct {
	pool   *pgxpool.Pool
	ctx    context.Context
	cellID int64
	size   int64
	mu     sync.Mutex
	cache  map[int32][]byte
}

func (r *chunkReader) Size() int64 {
	return r.size
}

func (r *chunkReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("layer: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	first := int32(off / 8120)
	end := min(off+int64(len(p)), r.size)
	last := int32((end - 1) / 8120)
	r.mu.Lock()
	defer r.mu.Unlock()
	missing := false
	for n := first; n <= last; n++ {
		if _, ok := r.cache[n]; !ok {
			missing = true
			break
		}
	}
	if missing {
		rows, err := r.pool.Query(r.ctx, `select no, data from pgpq.cell_chunks where cell_id = $1 and no between $2 and $3`, r.cellID, first, last)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var n int32
			var data []byte
			if err := rows.Scan(&n, &data); err != nil {
				rows.Close()
				return 0, err
			}
			r.cache[n] = data
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
	}
	written := 0
	for n := first; n <= last; n++ {
		data, ok := r.cache[n]
		if !ok {
			return written, io.ErrUnexpectedEOF
		}
		lo := int64(0)
		if n == first {
			lo = off - int64(n)*8120
		}
		hi := int64(len(data))
		if n == last {
			hi = min(hi, end-int64(n)*8120)
		}
		if lo < 0 || lo > hi {
			return written, io.ErrUnexpectedEOF
		}
		written += copy(p[written:], data[lo:hi])
	}
	if written < len(p) {
		return written, io.EOF
	}
	return written, nil
}
