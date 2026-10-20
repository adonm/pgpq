package layer

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/spatial"
)

var chunkColumns = []string{"collection", "cell", "first_key", "first_id", "last_key", "last_id", "minx", "miny", "maxx", "maxy", "features", "file_id", "col_file", "fence_file", "zone", "level"}

func chunkValues(name string, c chunkRow) []any {
	return []any{
		name, spatial.ToOrdered(c.cell),
		spatial.ToOrdered(c.first.key), c.first.id,
		spatial.ToOrdered(c.last.key), c.last.id,
		c.bounds[0], c.bounds[1], c.bounds[2], c.bounds[3],
		int32(c.rows), c.fileID, c.colFile, c.fenceFile, c.zone, c.level,
	}
}

func (l *Layer) commit(ctx context.Context, name string, ds ingest.Dataset, cells []uint64, chunks []chunkRow, entries []entry) (LoadResult, error) {
	return l.commitRows(ctx, name, ds.Columns, ds.IDColumn, cells, chunks, entries)
}

func (l *Layer) commitRows(ctx context.Context, name string, columns []chunk.Column, idColumn string, cells []uint64, chunks []chunkRow, entries []entry) (LoadResult, error) {
	encoded, err := encodeColumns(columns)
	if err != nil {
		return LoadResult{}, err
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return LoadResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into pfgb.collections (name, id_column, columns_json) values ($1, $2, $3) on conflict (name) do nothing`,
		name, idColumn, encoded); err != nil {
		return LoadResult{}, err
	}
	var uid int64
	var storedIDColumn string
	var stored []byte
	if err := tx.QueryRow(ctx, `select uid, coalesce(id_column, ''), columns_json from pfgb.collections where name = $1 for update`, name).
		Scan(&uid, &storedIDColumn, &stored); err != nil {
		return LoadResult{}, err
	}
	whole := cells == nil
	if !whole {
		schema, err := decodeColumns(stored)
		if err != nil {
			return LoadResult{}, err
		}
		if storedIDColumn != idColumn || !slices.Equal(schema, columns) {
			return LoadResult{}, invalidf("the load's schema differs from the collection's; reload the whole collection")
		}
	}
	touched := cells
	if whole {
		touched = nil
		for _, c := range chunks {
			touched = append(touched, c.cell)
		}
		slices.Sort(touched)
		touched = slices.Compact(touched)
	}
	replace := append([]uint64(nil), touched...)
	children := touched
	for _, step := range coarseSteps {
		parents := ancestors(children, step.level)
		replace = append(replace, parents...)
		children = parents
	}
	slices.Sort(replace)
	replace = slices.Compact(replace)
	if whole {
		if err := deleteAll(ctx, tx, name, uid); err != nil {
			return LoadResult{}, err
		}
	} else if err := deleteCells(ctx, tx, name, replace); err != nil {
		return LoadResult{}, err
	}
	rows := make([][]any, len(chunks))
	for i, c := range chunks {
		rows[i] = chunkValues(name, c)
	}
	if len(rows) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pfgb", "chunks"}, chunkColumns, pgx.CopyFromRows(rows)); err != nil {
			return LoadResult{}, err
		}
	}
	put := func(ctx context.Context, collection, kind, encoding string, data []byte) (int64, error) {
		return l.blocks.PutTxEncoded(ctx, tx, collection, kind, encoding, data)
	}
	coarse, err := l.rebuildCoarse(ctx, tx, put, Collection{Name: name, Columns: columns, IDColumn: idColumn}, touched)
	if err != nil {
		return LoadResult{}, err
	}
	if err := replaceIDs(ctx, tx, uid, touched, entries); err != nil {
		return LoadResult{}, err
	}
	if _, err := tx.Exec(ctx, `update pfgb.collections set version = version + 1, id_column = $2, columns_json = $3 where name = $1`,
		name, idColumn, encoded); err != nil {
		return LoadResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Collection: name, Features: len(entries), Chunks: len(chunks) + len(coarse), Partitions: len(touched)}, nil
}

func deleteCells(ctx context.Context, tx pgx.Tx, name string, cells []uint64) error {
	if len(cells) == 0 {
		return nil
	}
	ordered := orderedCells(cells)
	rows, err := tx.Query(ctx, `select file_id, col_file, fence_file from pfgb.chunks where collection = $1 and cell = any($2)`, name, ordered)
	if err != nil {
		return err
	}
	var files []int64
	for rows.Next() {
		var fgb, cols, fence int64
		if err := rows.Scan(&fgb, &cols, &fence); err != nil {
			rows.Close()
			return err
		}
		files = append(files, fgb, cols, fence)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(files) > 0 {
		if _, err := tx.Exec(ctx, `delete from pfgb.blocks where file_id = any($1)`, files); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from pfgb.files where file_id = any($1)`, files); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `delete from pfgb.chunks where collection = $1 and cell = any($2)`, name, ordered)
	return err
}

func deleteAll(ctx context.Context, tx pgx.Tx, name string, uid int64) error {
	rows, err := tx.Query(ctx, `select file_id, col_file, fence_file from pfgb.chunks where collection = $1`, name)
	if err != nil {
		return err
	}
	var files []int64
	for rows.Next() {
		var fgb, cols, fence int64
		if err := rows.Scan(&fgb, &cols, &fence); err != nil {
			rows.Close()
			return err
		}
		files = append(files, fgb, cols, fence)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(files) > 0 {
		if _, err := tx.Exec(ctx, `delete from pfgb.blocks where file_id = any($1)`, files); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from pfgb.files where file_id = any($1)`, files); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `delete from pfgb.chunks where collection = $1`, name); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `delete from pfgb.ids where collection_uid = $1`, uid)
	return err
}

func replaceIDs(ctx context.Context, tx pgx.Tx, uid int64, cells []uint64, entries []entry) error {
	for _, cell := range cells {
		lo, hi := spatial.Range(cell)
		if _, err := tx.Exec(ctx, `delete from pfgb.ids where collection_uid = $1 and key between $2 and $3`,
			uid, spatial.ToOrdered(lo), spatial.ToOrdered(hi)); err != nil {
			return err
		}
	}
	seen := make(map[[2]int64]bool, len(entries))
	rows := make([][]any, 0, len(entries))
	for _, e := range entries {
		h, key := int64(idHash(e.id)), spatial.ToOrdered(e.key)
		k := [2]int64{h, key}
		if seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, []any{uid, h, key})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"pfgb", "ids"}, []string{"collection_uid", "hash", "key"}, pgx.CopyFromRows(rows))
	return err
}

func orderedCells(cells []uint64) []int64 {
	out := make([]int64, len(cells))
	for i, cell := range cells {
		out[i] = spatial.ToOrdered(cell)
	}
	return out
}
