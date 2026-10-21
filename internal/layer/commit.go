package layer

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
)

var cellColumns = []string{"collection_uid", "level", "cell", "cell_id", "features", "bytes", "minx", "miny", "maxx", "maxy", "first_key", "first_id", "last_key", "last_id", "zone"}

func cellValues(uid int64, row cellRow) []any {
	return []any{uid, row.level, spatial.ToOrdered(row.cell), row.cellID, row.features, row.bytes, row.bounds[0], row.bounds[1], row.bounds[2], row.bounds[3],
		spatial.ToOrdered(row.firstKey), row.firstID, spatial.ToOrdered(row.lastKey), row.lastID, row.zone}
}

func (l *Layer) commit(ctx context.Context, name string, ds geo.Dataset, cells []uint64, rows []cellRow, detail map[uint64][]geo.Feature, replaceAll bool) (LoadResult, error) {
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
	if !replaceAll {
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
		touched = make([]uint64, 0, len(detail))
		for cell := range detail {
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
	if replaceAll {
		if err := deleteAll(ctx, tx, uid); err != nil {
			return LoadResult{}, err
		}
	} else if err := deleteCells(ctx, tx, uid, replace); err != nil {
		return LoadResult{}, err
	}
	ids := make([]int64, len(rows))
	if len(rows) > 0 {
		idRows, err := tx.Query(ctx, `select nextval('pgpq.cell_ids') from generate_series(1, $1)`, len(rows))
		if err != nil {
			return LoadResult{}, err
		}
		i := 0
		for idRows.Next() {
			if err := idRows.Scan(&ids[i]); err != nil {
				idRows.Close()
				return LoadResult{}, err
			}
			i++
		}
		err = idRows.Err()
		idRows.Close()
		if err != nil {
			return LoadResult{}, err
		}
	}
	for i := range rows {
		rows[i].cellID = ids[i]
	}
	values := make([][]any, len(rows))
	for i, row := range rows {
		values[i] = cellValues(uid, row)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "cells"}, cellColumns, pgx.CopyFromRows(values)); err != nil {
		return LoadResult{}, err
	}
	chunks := make([][]any, 0, len(rows))
	for _, row := range rows {
		for n := 0; n*chunkBytes < len(row.data); n++ {
			end := min((n+1)*chunkBytes, len(row.data))
			chunks = append(chunks, []any{row.cellID, n, row.data[n*chunkBytes : end]})
		}
	}
	if len(chunks) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "cell_chunks"}, []string{"cell_id", "no", "data"}, pgx.CopyFromRows(chunks)); err != nil {
			return LoadResult{}, err
		}
	}
	if err := replaceIDs(ctx, tx, uid, detail); err != nil {
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
	if _, err := tx.Exec(ctx, `delete from pgpq.cell_chunks where cell_id in (select cell_id from pgpq.cells where collection_uid = $1 and cell = any($2))`, uid, ordered); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from pgpq.cells where collection_uid = $1 and cell = any($2)`, uid, ordered); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `delete from pgpq.ids where collection_uid = $1 and cell = any($2)`, uid, ordered)
	return err
}

func deleteAll(ctx context.Context, tx pgx.Tx, uid int64) error {
	if _, err := tx.Exec(ctx, `delete from pgpq.cell_chunks where cell_id in (select cell_id from pgpq.cells where collection_uid = $1)`, uid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from pgpq.cells where collection_uid = $1`, uid); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `delete from pgpq.ids where collection_uid = $1`, uid)
	return err
}

func replaceIDs(ctx context.Context, tx pgx.Tx, uid int64, groups map[uint64][]geo.Feature) error {
	type idCell struct {
		cell     int64
		features []geo.Feature
	}
	cells := make([]idCell, 0, len(groups))
	total := 0
	for cell, features := range groups {
		if len(features) == 0 {
			continue
		}
		cells = append(cells, idCell{cell: spatial.ToOrdered(cell), features: features})
		total += len(features)
	}
	if total == 0 {
		return nil
	}
	cell, row := 0, 0
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "ids"}, []string{"collection_uid", "hash", "cell", "row"},
		pgx.CopyFromSlice(total, func(int) ([]any, error) {
			for row == len(cells[cell].features) {
				cell++
				row = 0
			}
			feature := cells[cell].features[row]
			values := []any{uid, int64(idHash(feature.ID)), cells[cell].cell, row}
			row++
			return values, nil
		}))
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
