package layer

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
)

var cellColumns = []string{"collection_uid", "level", "cell", "features", "bytes", "minx", "miny", "maxx", "maxy", "first_key", "first_id", "last_key", "last_id", "zone", "data"}

func cellValues(uid int64, row cellRow) []any {
	return []any{uid, row.level, spatial.ToOrdered(row.cell), row.features, row.bytes, row.bounds[0], row.bounds[1], row.bounds[2], row.bounds[3],
		spatial.ToOrdered(row.firstKey), row.firstID, spatial.ToOrdered(row.lastKey), row.lastID, row.zone, row.data}
}

func (l *Layer) commit(ctx context.Context, name string, ds geo.Dataset, cells []uint64, rows []cellRow, groups map[uint64][]entry) (LoadResult, error) {
	columns, err := encodeColumns(ds.Columns)
	if err != nil {
		return LoadResult{}, err
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return LoadResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into pgpq.collections (name, id_column, geometry, columns_json) values ($1, $2, $3, $4) on conflict (name) do nothing`,
		name, ds.IDColumn, ds.Geometry, columns); err != nil {
		return LoadResult{}, err
	}
	var uid, version int64
	var storedID, storedGeometry string
	var stored []byte
	if err := tx.QueryRow(ctx, `select uid, version, coalesce(id_column, ''), geometry, columns_json from pgpq.collections where name = $1 for update`, name).
		Scan(&uid, &version, &storedID, &storedGeometry, &stored); err != nil {
		return LoadResult{}, err
	}
	if cells != nil {
		schema, err := decodeColumns(stored)
		if err != nil {
			return LoadResult{}, err
		}
		if storedID != ds.IDColumn || storedGeometry != ds.Geometry || !slices.Equal(schema, ds.Columns) {
			return LoadResult{}, invalidf("the load's schema differs from the collection's; reload the whole collection")
		}
	}
	touched := cells
	if touched == nil {
		touched = make([]uint64, 0, len(groups))
		for cell := range groups {
			touched = append(touched, cell)
		}
	}
	slices.Sort(touched)
	touched = slices.Compact(touched)
	replace := append([]uint64(nil), touched...)
	children := touched
	for _, step := range coarseSteps {
		parents := ancestors(children, step.level)
		replace = append(replace, parents...)
		children = parents
	}
	slices.Sort(replace)
	replace = slices.Compact(replace)
	if cells == nil {
		if err := deleteAll(ctx, tx, uid); err != nil {
			return LoadResult{}, err
		}
	} else if err := deleteCells(ctx, tx, uid, replace); err != nil {
		return LoadResult{}, err
	}
	values := make([][]any, len(rows))
	for i, row := range rows {
		values[i] = cellValues(uid, row)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "cells"}, cellColumns, pgx.CopyFromRows(values)); err != nil {
		return LoadResult{}, err
	}
	if err := replaceIDs(ctx, tx, uid, groups); err != nil {
		return LoadResult{}, err
	}
	if _, err := tx.Exec(ctx, `update pgpq.collections set version = version + 1, id_column = $2, geometry = $3, columns_json = $4 where name = $1`,
		name, ds.IDColumn, ds.Geometry, columns); err != nil {
		return LoadResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Collection: name, Features: len(ds.Features), Cells: len(rows), Partitions: len(touched)}, nil
}

func deleteCells(ctx context.Context, tx pgx.Tx, uid int64, cells []uint64) error {
	if len(cells) == 0 {
		return nil
	}
	ordered := orderedCells(cells)
	if _, err := tx.Exec(ctx, `delete from pgpq.cells where collection_uid = $1 and cell = any($2)`, uid, ordered); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `delete from pgpq.ids where collection_uid = $1 and cell = any($2)`, uid, ordered)
	return err
}

func deleteAll(ctx context.Context, tx pgx.Tx, uid int64) error {
	if _, err := tx.Exec(ctx, `delete from pgpq.cells where collection_uid = $1`, uid); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `delete from pgpq.ids where collection_uid = $1`, uid)
	return err
}

func replaceIDs(ctx context.Context, tx pgx.Tx, uid int64, groups map[uint64][]entry) error {
	var values [][]any
	for cell, entries := range groups {
		for row := range entries {
			values = append(values, []any{uid, int64(entries[row].hash), spatial.ToOrdered(cell), row})
		}
	}
	if len(values) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "ids"}, []string{"collection_uid", "hash", "cell", "row"}, pgx.CopyFromRows(values))
	return err
}

func ancestors(cells []uint64, level int) []uint64 {
	seen := map[uint64]bool{}
	var out []uint64
	for _, cell := range cells {
		parent := spatial.Level(cell, level)
		if !seen[parent] {
			seen[parent] = true
			out = append(out, parent)
		}
	}
	slices.Sort(out)
	return out
}
