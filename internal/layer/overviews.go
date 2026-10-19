package layer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/spatial"
)

type chunkSample struct {
	entry entry
	input chunk.InputFeature
}

func (l *Layer) rebuildCoarse(ctx context.Context, tx pgx.Tx, put blobPut, coll Collection, dirty []uint64) ([]chunkRow, error) {
	var all []chunkRow
	children := dirty
	for _, step := range coarseSteps {
		parents := ancestors(children, step.level)
		var rows []chunkRow
		for _, parent := range parents {
			built, err := l.coarseChunks(ctx, tx, put, coll, parent, step.level, step.from)
			if err != nil {
				return nil, err
			}
			rows = append(rows, built...)
		}
		if err := insertChunks(ctx, tx, coll.Name, rows); err != nil {
			return nil, err
		}
		all = append(all, rows...)
		children = parents
	}
	return all, nil
}

func (l *Layer) coarseChunks(ctx context.Context, tx pgx.Tx, put blobPut, coll Collection, parent uint64, level, from int) ([]chunkRow, error) {
	lo, hi := spatial.Range(parent)
	fileRows, err := tx.Query(ctx, `select file_id from pfgb.chunks where collection = $1 and level = $2 and cell between $3 and $4`,
		coll.Name, from, spatial.ToOrdered(lo), spatial.ToOrdered(hi))
	if err != nil {
		return nil, err
	}
	var fileIDs []int64
	for fileRows.Next() {
		var id int64
		if err := fileRows.Scan(&id); err != nil {
			fileRows.Close()
			return nil, err
		}
		fileIDs = append(fileIDs, id)
	}
	fileRows.Close()
	if err := fileRows.Err(); err != nil {
		return nil, err
	}
	var samples []chunkSample
	for _, fileID := range fileIDs {
		data, err := l.blocks.ReadFrom(ctx, tx, fileID)
		if err != nil {
			return nil, err
		}
		c, err := chunk.Open(data)
		if err != nil {
			return nil, err
		}
		features, err := c.SearchAll()
		if err != nil {
			return nil, err
		}
		cols := c.Columns()
		for _, f := range features {
			bounds, err := featureBounds(c, f)
			if err != nil {
				return nil, err
			}
			id, err := storedID(f, cols, coll.IDColumn)
			if err != nil {
				return nil, err
			}
			input, err := inputOf(c, f)
			if err != nil {
				return nil, err
			}
			samples = append(samples, chunkSample{entry: entry{key: spatial.Key(bounds), id: id, hash: idHash(id), bounds: bounds}, input: input})
		}
	}
	slices.SortFunc(samples, func(a, b chunkSample) int {
		if c := cmp.Compare(a.entry.hash, b.entry.hash); c != 0 {
			return c
		}
		return compareKeyID(a.entry.key, a.entry.id, b.entry.key, b.entry.id)
	})
	samples = samples[:min(len(samples), overviewCap)]
	slices.SortFunc(samples, func(a, b chunkSample) int {
		return compareKeyID(a.entry.key, a.entry.id, b.entry.key, b.entry.id)
	})
	if len(samples) == 0 {
		return nil, nil
	}
	inputs := make([]chunk.InputFeature, len(samples))
	entries := make([]entry, len(samples))
	for i, s := range samples {
		inputs[i] = s.input
		entries[i] = s.entry
		entries[i].cell = parent
		entries[i].index = i
	}
	ds := ingest.Dataset{Columns: coll.Columns, IDColumn: coll.IDColumn, Features: inputs}
	rows, err := l.writeRange(ctx, put, coll.Name, ds, parent, entries, level)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].seq = i
	}
	return rows, nil
}

func inputOf(c *chunk.Chunk, f *flattypes.Feature) (chunk.InputFeature, error) {
	feature := c.GeoJSON(f)
	if feature.Geometry == nil || feature.Geometry.Coordinates == nil {
		return chunk.InputFeature{}, errors.New("layer: overview source has no coordinates")
	}
	coords, err := json.Marshal(feature.Geometry.Coordinates)
	if err != nil {
		return chunk.InputFeature{}, err
	}
	properties := map[string]json.RawMessage{}
	for name, value := range chunk.DecodeProperties(f.PropertiesBytes(), c.Columns()) {
		raw, err := json.Marshal(value)
		if err != nil {
			return chunk.InputFeature{}, err
		}
		properties[name] = raw
	}
	return chunk.InputFeature{Geometry: chunk.RawGeometry{Type: feature.Geometry.Type, Coordinates: coords}, Properties: properties}, nil
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

func insertChunks(ctx context.Context, tx pgx.Tx, name string, rows []chunkRow) error {
	if len(rows) == 0 {
		return nil
	}
	values := make([][]any, len(rows))
	for i, c := range rows {
		values[i] = chunkValues(name, c)
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"pfgb", "chunks"}, chunkColumns, pgx.CopyFromRows(values))
	return err
}
