package dataset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/store"
)

const chunkBytes = 8120

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type shardRecord struct {
	ID          int64
	Seq         int
	Rows        int
	Bytes       int64
	DataStart   int64
	DataEnd     int64
	FirstKey    int64
	LastKey     int64
	VirtualBase int64
}

func shardReader(tx pgx.Tx, pool *pgxpool.Pool, id int64, size int64) *chunkReader {
	return &chunkReader{tx: tx, pool: pool, shardID: id, size: size, cache: make(map[int32][]byte)}
}

type chunkReader struct {
	tx      pgx.Tx
	pool    *pgxpool.Pool
	shardID int64
	size    int64
	mu      sync.Mutex
	cache   map[int32][]byte
}

func (r *chunkReader) Size() int64 { return r.size }

func (r *chunkReader) query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if r.tx != nil {
		return r.tx.Query(ctx, sql, args...)
	}
	return r.pool.Query(ctx, sql, args...)
}

func (r *chunkReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("dataset: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	first := int32(off / chunkBytes)
	end := min(off+int64(len(p)), r.size)
	last := int32((end - 1) / chunkBytes)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) > 4096 {
		r.cache = make(map[int32][]byte)
	}
	missing := false
	for n := first; n <= last; n++ {
		if _, ok := r.cache[n]; !ok {
			missing = true
			break
		}
	}
	if missing {
		rows, err := r.query(context.Background(), `select no, data from pgpq.shard_chunks where shard_id = $1 and no between $2 and $3`, r.shardID, first, last)
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
			lo = off - int64(n)*chunkBytes
		}
		hi := int64(len(data))
		if n == last {
			hi = min(hi, end-int64(n)*chunkBytes)
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

func loadShardBytes(ctx context.Context, q querier, id int64, size int64) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(int(size))
	rows, err := q.Query(ctx, `select no, data from pgpq.shard_chunks where shard_id = $1 order by no`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	want := int32(0)
	for rows.Next() {
		var no int32
		var data []byte
		if err := rows.Scan(&no, &data); err != nil {
			return nil, err
		}
		if no != want {
			return nil, errors.New("dataset: missing shard chunk")
		}
		out.Write(data)
		want++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(out.Len()) != size {
		return nil, errors.New("dataset: shard size mismatch")
	}
	return out.Bytes(), nil
}

func loadShardDataset(ctx context.Context, q querier, shard shardRecord, idColumn string) (geo.Dataset, error) {
	raw, err := loadShardBytes(ctx, q, shard.ID, shard.Bytes)
	if err != nil {
		return geo.Dataset{}, err
	}
	return geo.ReadDataset(bytes.NewReader(raw), geo.Options{IDColumn: idColumn})
}

func insertShard(ctx context.Context, tx pgx.Tx, collection string, data []byte) (shardRecord, error) {
	if len(data) < 12 || string(data[:4]) != "PAR1" || string(data[len(data)-4:]) != "PAR1" {
		return shardRecord{}, invalidf("shard is not a Parquet file")
	}
	footerLen := int64(binary.LittleEndian.Uint32(data[len(data)-8 : len(data)-4]))
	dataEnd := int64(len(data)) - 8 - footerLen
	if dataEnd <= 4 || dataEnd >= int64(len(data)) {
		return shardRecord{}, invalidf("shard has an invalid footer")
	}
	shard := shardRecord{DataStart: 4, DataEnd: dataEnd, Bytes: int64(len(data))}
	if err := tx.QueryRow(ctx,
		`insert into pgpq.shards (collection, bytes, data_start, data_end, rows, first_key, last_key) values ($1, $2, $3, $4, 0, 0, 0) returning shard_id`,
		collection, shard.Bytes, shard.DataStart, shard.DataEnd).Scan(&shard.ID); err != nil {
		return shardRecord{}, err
	}
	chunks := make([][]any, 0, (len(data)+chunkBytes-1)/chunkBytes)
	for n := 0; n*chunkBytes < len(data); n++ {
		end := min((n+1)*chunkBytes, len(data))
		chunks = append(chunks, []any{shard.ID, n, data[n*chunkBytes : end]})
	}
	if len(chunks) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "shard_chunks"}, []string{"shard_id", "no", "data"}, pgx.CopyFromRows(chunks)); err != nil {
			return shardRecord{}, err
		}
	}
	return shard, nil
}

func setShardRange(ctx context.Context, tx pgx.Tx, shard shardRecord, first, last int64, rows int) error {
	shard.FirstKey, shard.LastKey, shard.Rows = first, last, rows
	_, err := tx.Exec(ctx, `update pgpq.shards set first_key = $2, last_key = $3, rows = $4 where shard_id = $1`,
		shard.ID, first, last, rows)
	return err
}

func (m *Manager) readShards(ctx context.Context, q querier, collection string, version int64) ([]shardRecord, error) {
	rows, err := q.Query(ctx, `
		select s.shard_id, cs.seq, s.rows, s.bytes, s.data_start, s.data_end, s.first_key, s.last_key, cs.virtual_base
		from pgpq.collection_shards cs
		join pgpq.shards s using (shard_id)
		where cs.collection = $1 and cs.version = $2
		order by cs.seq`, collection, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardRecord
	for rows.Next() {
		var s shardRecord
		if err := rows.Scan(&s.ID, &s.Seq, &s.Rows, &s.Bytes, &s.DataStart, &s.DataEnd, &s.FirstKey, &s.LastKey, &s.VirtualBase); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func shardIDs(shards []shardRecord) []int64 {
	out := make([]int64, len(shards))
	for i, shard := range shards {
		out[i] = shard.ID
	}
	return out
}

func shardDataLength(s shardRecord) int64 {
	return s.DataEnd - s.DataStart
}

func shardByID(shards []shardRecord, id int64) (shardRecord, bool) {
	for _, shard := range shards {
		if shard.ID == id {
			return shard, true
		}
	}
	return shardRecord{}, false
}

func (m *Manager) Open(ctx context.Context, name string) (*VirtualFile, error) {
	coll, err := m.Collection(ctx, name)
	if err != nil {
		return nil, err
	}
	return m.OpenVersion(ctx, name, coll.Version)
}

func (m *Manager) OpenVersion(ctx context.Context, name string, version int64) (*VirtualFile, error) {
	var footer []byte
	var footerStart, totalLength int64
	var etag string
	err := m.pool.QueryRow(ctx, `select footer, footer_start, total_length, etag from pgpq.virtual_files where collection = $1 and version = $2`, name, version).
		Scan(&footer, &footerStart, &totalLength, &etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	shards, err := m.readShards(ctx, m.pool, name, version)
	if err != nil {
		return nil, err
	}
	return &VirtualFile{
		collection:  name,
		version:     version,
		etag:        etag,
		footer:      footer,
		footerStart: footerStart,
		totalLength: totalLength,
		shards:      shards,
		pool:        m.pool,
	}, nil
}

func fmtShardError(id int64, err error) error {
	return fmt.Errorf("dataset: shard %d: %w", id, err)
}
