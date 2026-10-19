package blocks

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Size = 8120

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Put(ctx context.Context, collection, kind string, data []byte) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
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
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) Publish(ctx context.Context, tx pgx.Tx, fileIDs []int64) error {
	_, err := tx.Exec(ctx, `update pfgb.files set published = true where file_id = any($1)`, fileIDs)
	return err
}

func (s *Store) ReadAll(ctx context.Context, fileID int64) ([]byte, error) {
	var size int64
	if err := s.pool.QueryRow(ctx, `select bytes from pfgb.files where file_id = $1`, fileID).Scan(&size); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `select n, data from pfgb.blocks where file_id = $1 order by n`, fileID)
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
