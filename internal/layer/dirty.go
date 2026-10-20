package layer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/spatial"
)

const (
	dirtyQuiet    = 5 * time.Second
	dirtyBatch    = 32
	dirtyFlushCap = 1024
)

func queueOverviews(ctx context.Context, tx pgx.Tx, name string, cells []uint64, level int) error {
	if len(cells) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `insert into pfgb.overview_dirty (collection, cell, level)
		select $1, cell, $3 from unnest($2::bigint[]) as pending (cell)
		on conflict (collection, cell, level) do nothing`, name, orderedCells(cells), level)
	return err
}

func (l *Layer) RunDirty(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.flushDirty(ctx, false, dirtyBatch); err != nil && ctx.Err() == nil {
				slog.Error("overview worker", "error", err)
			}
		}
	}
}

func (l *Layer) FlushDirty(ctx context.Context) error {
	return l.flushDirty(ctx, true, dirtyFlushCap)
}

func (l *Layer) flushDirty(ctx context.Context, force bool, limit int) error {
	for processed := 0; processed < limit; processed++ {
		done, err := l.flushDirtyRow(ctx, force)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return nil
}

func (l *Layer) flushDirtyRow(ctx context.Context, force bool) (bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var name string
	var cell int64
	var level int
	err = tx.QueryRow(ctx, `select collection, cell, level from pfgb.overview_dirty
		where $1 or dirty_since <= now() - $2::interval
		order by dirty_since limit 1`, force, dirtyQuiet.String()).Scan(&name, &cell, &level)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	coll, err := collectionTx(ctx, tx, name)
	if err != nil {
		return false, err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from pfgb.overview_dirty where collection = $1 and cell = $2 and level = $3)`, name, cell, level).Scan(&pending); err != nil {
		return false, err
	}
	if !pending {
		return false, nil
	}
	from := inferredSource(level)
	if from == 0 {
		if _, err := tx.Exec(ctx, `delete from pfgb.overview_dirty where collection = $1 and cell = $2 and level = $3`, name, cell, level); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	parent := spatial.FromOrdered(cell)
	if err := deleteCells(ctx, tx, name, []uint64{parent}); err != nil {
		return false, err
	}
	put := func(ctx context.Context, collection, kind, encoding string, data []byte) (int64, error) {
		return l.blocks.PutTxEncoded(ctx, tx, collection, kind, encoding, data)
	}
	rows, err := l.coarseChunks(ctx, tx, put, coll, parent, level, from)
	if err != nil {
		return false, err
	}
	if err := insertChunks(ctx, tx, name, rows); err != nil {
		return false, err
	}
	fileIDs := make([]int64, 0, len(rows)*3)
	for _, row := range rows {
		fileIDs = append(fileIDs, row.fileID, row.colFile, row.fenceFile)
	}
	if err := l.blocks.Publish(ctx, tx, fileIDs); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `delete from pfgb.overview_dirty where collection = $1 and cell = $2 and level = $3`, name, cell, level); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `update pfgb.collections set version = version + 1 where name = $1`, name); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

func inferredSource(level int) int {
	switch level {
	case 6:
		return detailLevel
	case 4:
		return 6
	case 2:
		return 4
	}
	return 0
}

func collectionTx(ctx context.Context, tx pgx.Tx, name string) (Collection, error) {
	var coll Collection
	var columns []byte
	err := tx.QueryRow(ctx, `select uid, version, coalesce(id_column, ''), columns_json from pfgb.collections where name = $1 for update`, name).
		Scan(&coll.UID, &coll.Version, &coll.IDColumn, &columns)
	if err != nil {
		return coll, err
	}
	coll.Name = name
	coll.Columns, err = decodeColumns(columns)
	return coll, err
}
