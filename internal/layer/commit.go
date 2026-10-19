package layer

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/spatial"
)

func (l *Layer) commit(ctx context.Context, name string, ds ingest.Dataset, cells []uint64, chunks []chunkRow, runFile int64, features int) (LoadResult, error) {
	columns, err := encodeColumns(ds.Columns)
	if err != nil {
		return LoadResult{}, err
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return LoadResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into pfgb.collections (name, id_column, columns_json) values ($1, $2, $3) on conflict (name) do nothing`,
		name, ds.IDColumn, columns); err != nil {
		return LoadResult{}, err
	}
	var current int64
	var idColumn string
	var stored []byte
	if err := tx.QueryRow(ctx, `select current_snapshot, coalesce(id_column, ''), columns_json from pfgb.collections where name = $1 for update`, name).
		Scan(&current, &idColumn, &stored); err != nil {
		return LoadResult{}, err
	}
	whole := cells == nil
	if !whole && current > 0 {
		schema, err := decodeColumns(stored)
		if err != nil {
			return LoadResult{}, err
		}
		if idColumn != ds.IDColumn || !slices.Equal(schema, ds.Columns) {
			return LoadResult{}, invalidf("the load's schema differs from the collection's; reload the whole collection")
		}
	}
	snapshot := current + 1
	touched := cells
	if whole {
		if _, err := tx.Exec(ctx, `update pfgb.chunks set end_snapshot = $2 where collection = $1 and end_snapshot is null`, name, snapshot); err != nil {
			return LoadResult{}, err
		}
		if _, err := tx.Exec(ctx, `update pfgb.partitions set changed = $2 where collection = $1`, name, snapshot); err != nil {
			return LoadResult{}, err
		}
		if _, err := tx.Exec(ctx, `update pfgb.id_runs set end_snapshot = $2 where collection = $1 and end_snapshot is null`, name, snapshot); err != nil {
			return LoadResult{}, err
		}
		touched = nil
		for _, c := range chunks {
			touched = append(touched, c.cell)
		}
		slices.Sort(touched)
		touched = slices.Compact(touched)
	} else {
		ordered := orderedCells(cells)
		if _, err := tx.Exec(ctx, `update pfgb.chunks set end_snapshot = $3 where collection = $1 and cell = any($2) and end_snapshot is null`, name, ordered, snapshot); err != nil {
			return LoadResult{}, err
		}
	}
	if len(touched) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into pfgb.partitions (collection, cell, changed)
			select $1, unnest($2::bigint[]), $3
			on conflict (collection, cell) do update set changed = excluded.changed`,
			name, orderedCells(touched), snapshot); err != nil {
			return LoadResult{}, err
		}
	}
	rows := make([][]any, len(chunks))
	fileIDs := append(make([]int64, 0, len(chunks)+1), runFile)
	for i, c := range chunks {
		rows[i] = []any{
			name, spatial.ToOrdered(c.cell), int32(c.seq),
			spatial.ToOrdered(c.first.key), c.first.id,
			spatial.ToOrdered(c.last.key), c.last.id,
			c.bounds[0], c.bounds[1], c.bounds[2], c.bounds[3],
			int32(c.rows), c.fileID, snapshot, c.colFile, c.zone, c.level,
		}
		fileIDs = append(fileIDs, c.fileID, c.colFile)
	}
	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pfgb", "chunks"},
			[]string{"collection", "cell", "seq", "first_key", "first_id", "last_key", "last_id", "minx", "miny", "maxx", "maxy", "features", "file_id", "begin_snapshot", "col_file", "zone", "level"},
			pgx.CopyFromRows(rows)); err != nil {
			return LoadResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `insert into pfgb.snapshots (collection, id) values ($1, $2)`, name, snapshot); err != nil {
		return LoadResult{}, err
	}
	if _, err := tx.Exec(ctx, `insert into pfgb.id_runs (file_id, collection, entries, begin_snapshot) values ($1, $2, $3, $4)`,
		runFile, name, int64(features), snapshot); err != nil {
		return LoadResult{}, err
	}
	if _, err := tx.Exec(ctx, `update pfgb.collections set current_snapshot = $2, id_column = $3, columns_json = $4 where name = $1`,
		name, snapshot, ds.IDColumn, columns); err != nil {
		return LoadResult{}, err
	}
	if err := l.blocks.Publish(ctx, tx, fileIDs); err != nil {
		return LoadResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Collection: name, Snapshot: snapshot, Features: features, Chunks: len(chunks), Partitions: len(touched)}, nil
}

func orderedCells(cells []uint64) []int64 {
	out := make([]int64, len(cells))
	for i, cell := range cells {
		out[i] = spatial.ToOrdered(cell)
	}
	return out
}
