package blocks

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/adonm/postfgb/internal/lru"
)

const Size = 8120

var ErrNotFound = errors.New("blocks: file not found")

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct {
	pool  *pgxpool.Pool
	cache *lru.Cache[int64, []byte]
	loads singleflight.Group
}

func New(pool *pgxpool.Pool, cacheBytes int64) *Store {
	s := &Store{pool: pool}
	if cacheBytes > 0 {
		s.cache = lru.New[int64, []byte](cacheBytes, func(data []byte) int64 { return int64(len(data)) })
	}
	return s
}

func (s *Store) Put(ctx context.Context, collection, kind string, data []byte) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	id, err := s.PutTx(ctx, tx, collection, kind, data)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) PutTx(ctx context.Context, tx pgx.Tx, collection, kind string, data []byte) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `insert into pfgb.files (collection, kind, bytes) values ($1, $2, $3) returning file_id`,
		collection, kind, int64(len(data))).Scan(&id); err != nil {
		return 0, err
	}
	rows := make([][]any, 0, (len(data)+Size-1)/Size)
	for n := 0; n*Size < len(data); n++ {
		end := min((n+1)*Size, len(data))
		rows = append(rows, []any{id, int32(n), data[n*Size : end]})
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
	if err := q.QueryRow(ctx, `select bytes from pfgb.files where file_id = $1`, fileID).Scan(&size); err != nil {
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
