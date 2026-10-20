package blocks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/singleflight"

	"github.com/adonm/postfgb/internal/lru"
)

const Size = 8120

const EncodingZstdBlock = "zstd-block"

var ErrNotFound = errors.New("blocks: file not found")

var (
	blockEncoder = newBlockEncoder()
	blockDecoder = newBlockDecoder()
)

func newBlockEncoder() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		panic(err)
	}
	return enc
}

func newBlockDecoder() *zstd.Decoder {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		panic(err)
	}
	return dec
}

func compressBlock(data []byte) []byte {
	return blockEncoder.EncodeAll(data, nil)
}

func decompressBlock(data []byte) ([]byte, error) {
	return blockDecoder.DecodeAll(data, nil)
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type blockKey struct {
	fileID int64
	n      int32
}

type Store struct {
	pool  *pgxpool.Pool
	cache *lru.Cache[int64, []byte]
	block *lru.Cache[blockKey, []byte]
	loads singleflight.Group
}

func New(pool *pgxpool.Pool, cacheBytes int64) *Store {
	s := &Store{pool: pool}
	if cacheBytes > 0 {
		s.cache = lru.New[int64, []byte](cacheBytes, func(data []byte) int64 { return int64(len(data)) })
		s.block = lru.New[blockKey, []byte](cacheBytes/2, func(data []byte) int64 { return int64(len(data)) })
	}
	return s
}

func (s *Store) Put(ctx context.Context, collection, kind string, data []byte) (int64, error) {
	return s.PutEncoded(ctx, collection, kind, "", data)
}

func (s *Store) PutEncoded(ctx context.Context, collection, kind, encoding string, data []byte) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	id, err := s.PutTxEncoded(ctx, tx, collection, kind, encoding, data)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) PutTx(ctx context.Context, tx pgx.Tx, collection, kind string, data []byte) (int64, error) {
	return s.PutTxEncoded(ctx, tx, collection, kind, "", data)
}

func (s *Store) PutTxEncoded(ctx context.Context, tx pgx.Tx, collection, kind, encoding string, data []byte) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `insert into pfgb.files (collection, kind, bytes, encoding) values ($1, $2, $3, $4) returning file_id`,
		collection, kind, int64(len(data)), encoding).Scan(&id); err != nil {
		return 0, err
	}
	rows := make([][]any, 0, (len(data)+Size-1)/Size)
	for n := 0; n*Size < len(data); n++ {
		end := min((n+1)*Size, len(data))
		block := data[n*Size : end]
		if encoding == EncodingZstdBlock {
			block = compressBlock(block)
		}
		rows = append(rows, []any{id, int32(n), block})
	}
	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pfgb", "blocks"}, []string{"file_id", "n", "data"}, pgx.CopyFromRows(rows)); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (s *Store) Publish(ctx context.Context, tx pgx.Tx, fileIDs []int64) error {
	_, err := tx.Exec(ctx, `update pfgb.files set published = true where file_id = any($1)`, fileIDs)
	return err
}

func (s *Store) ReadAll(ctx context.Context, fileID int64) ([]byte, error) {
	if s.cache != nil {
		if data, ok := s.cache.Get(fileID); ok {
			return data, nil
		}
	}
	shared, err, _ := s.loads.Do(strconv.FormatInt(fileID, 10), func() (any, error) {
		data, err := readFile(context.WithoutCancel(ctx), s.pool, fileID)
		if err == nil && s.cache != nil {
			s.cache.Add(fileID, data)
		}
		return data, err
	})
	if err != nil {
		return nil, err
	}
	return shared.([]byte), nil
}

func (s *Store) ReadFrom(ctx context.Context, q querier, fileID int64) ([]byte, error) {
	return readFile(ctx, q, fileID)
}

func readFile(ctx context.Context, q querier, fileID int64) ([]byte, error) {
	var size int64
	var encoding string
	if err := q.QueryRow(ctx, `select bytes, encoding from pfgb.files where file_id = $1`, fileID).Scan(&size, &encoding); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := q.Query(ctx, `select n, data from pfgb.blocks where file_id = $1 order by n`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]byte, 0, size)
	next := int32(0)
	for rows.Next() {
		var n int32
		var data []byte
		if err := rows.Scan(&n, &data); err != nil {
			return nil, err
		}
		if n != next {
			return nil, fmt.Errorf("blocks: file %d is missing block %d", fileID, next)
		}
		if encoding == EncodingZstdBlock {
			data, err = decompressBlock(data)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, data...)
		next++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != size {
		return nil, fmt.Errorf("blocks: file %d has %d bytes, want %d", fileID, len(out), size)
	}
	return out, nil
}

type Reader struct {
	store    *Store
	ctx      context.Context
	fileID   int64
	size     int64
	encoding string
}

func (s *Store) Open(ctx context.Context, fileID int64) (*Reader, error) {
	var size int64
	var encoding string
	if err := s.pool.QueryRow(ctx, `select bytes, encoding from pfgb.files where file_id = $1`, fileID).Scan(&size, &encoding); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Reader{store: s, ctx: ctx, fileID: fileID, size: size, encoding: encoding}, nil
}

func (r *Reader) Size() int64 { return r.size }

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("blocks: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	if end > r.size {
		end = r.size
	}
	if r.store.cache != nil {
		if data, ok := r.store.cache.Get(r.fileID); ok {
			n := copy(p, data[off:end])
			if int64(n) < int64(len(p)) {
				return n, io.EOF
			}
			return n, nil
		}
	}
	first := int32(off / Size)
	last := int32((end - 1) / Size)
	fetched := make(map[int32][]byte, last-first+1)
	need := make([]int32, 0, last-first+1)
	for n := first; n <= last; n++ {
		if r.store.block != nil {
			if _, ok := r.store.block.Get(blockKey{r.fileID, n}); ok {
				continue
			}
		}
		need = append(need, n)
	}
	if len(need) > 0 {
		rows, err := r.store.pool.Query(r.ctx, `select n, data from pfgb.blocks where file_id = $1 and n between $2 and $3 order by n`, r.fileID, first, last)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var n int32
			var data []byte
			if err := rows.Scan(&n, &data); err != nil {
				return 0, err
			}
			if r.encoding == EncodingZstdBlock {
				data, err = decompressBlock(data)
				if err != nil {
					return 0, err
				}
			}
			fetched[n] = data
			if r.store.block != nil {
				r.store.block.Add(blockKey{r.fileID, n}, data)
			}
		}
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}
	written := 0
	for n := first; n <= last; n++ {
		var data []byte
		if r.store.block != nil {
			if b, ok := r.store.block.Get(blockKey{r.fileID, n}); ok {
				data = b
			}
		}
		if data == nil {
			data = fetched[n]
		}
		if data == nil {
			return written, fmt.Errorf("blocks: file %d is missing block %d", r.fileID, n)
		}
		lo := int64(0)
		if n == first {
			lo = off - int64(n)*Size
		}
		hi := int64(len(data))
		if n == last {
			hi = end - int64(n)*Size
		}
		copied := copy(p[written:], data[lo:hi])
		written += copied
	}
	if written < len(p) {
		return written, io.EOF
	}
	return written, nil
}
