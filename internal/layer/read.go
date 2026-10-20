package layer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/blocks"
	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/spatial"
	"github.com/adonm/postfgb/internal/stats"
)

type Query struct {
	BBox   *[4]float64
	Filter cql.Expr
	Level  int
	Limit  int
	Cursor *Cursor
}

type Hit struct {
	Key     uint64
	ID      string
	Chunk   *chunk.Chunk
	Feature *flattypes.Feature
}

type Page struct {
	Collection string
	Hits       []Hit
	Next       *Cursor
}

type chunkMeta struct {
	fileID    int64
	colFile   int64
	fenceFile int64
	features  int
	bounds    [4]float64
	zone      string
	firstKey  uint64
	firstID   string
	lastKey   uint64
	lastID    string
}

type located struct {
	hit    Hit
	bounds [4]float64
}

type catalogKey struct {
	collection string
	uid        int64
	version    int64
	level      int
	cell       uint64
}

const maxCatalogCells = 256

func (l *Layer) Items(ctx context.Context, name string, q Query) (Page, error) {
	page, err := l.items(ctx, name, q, false)
	if errors.Is(err, ErrStale) {
		page, err = l.items(ctx, name, q, true)
	}
	return page, err
}

func (l *Layer) items(ctx context.Context, name string, q Query, fresh bool) (Page, error) {
	if q.Limit <= 0 {
		return Page{}, invalidf("limit must be positive")
	}
	if q.Level != detailLevel && !slices.Contains(servedLevels, q.Level) {
		return Page{}, invalidf("overview level %d is not served", q.Level)
	}
	if q.Level != detailLevel && q.Filter != nil {
		return Page{}, invalidf("overview is served without a filter")
	}
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return Page{}, err
	}
	if q.Cursor != nil && (q.Cursor.UID != coll.UID || q.Cursor.Level != q.Level) {
		return Page{}, ErrBadCursor
	}
	var cells []uint64
	if q.BBox != nil {
		cells = spatial.Cover(*q.BBox, catalogCoverLevel(q.Level))
	}
	metas, err := l.catalogFor(ctx, coll, q.Level, cells, fresh)
	if err != nil {
		return Page{}, err
	}
	page := Page{Collection: name}
	for _, meta := range metas {
		if len(page.Hits) == q.Limit {
			break
		}
		if q.BBox != nil && !intersects(meta.bounds, q.BBox) {
			continue
		}
		if q.Cursor != nil && compareKeyID(meta.lastKey, meta.lastID, q.Cursor.Key, q.Cursor.ID) <= 0 {
			continue
		}
		if q.Filter != nil {
			zone, err := decodeZone(meta.zone)
			if err != nil {
				return Page{}, err
			}
			if !mayMatch(q.Filter, zone) {
				continue
			}
		}
		hits, err := l.matches(ctx, coll.IDColumn, meta, q, q.Limit-len(page.Hits))
		if err != nil {
			return Page{}, err
		}
		for _, hit := range hits {
			if len(page.Hits) == q.Limit {
				break
			}
			page.Hits = append(page.Hits, hit)
		}
	}
	if len(page.Hits) == q.Limit {
		last := page.Hits[len(page.Hits)-1]
		page.Next = &Cursor{UID: coll.UID, Level: q.Level, Key: last.Key, ID: last.ID}
	}
	return page, nil
}

func (l *Layer) Count(ctx context.Context, name string, filter cql.Expr) (int64, error) {
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return 0, err
	}
	metas, err := l.catalog(ctx, coll, detailLevel)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, meta := range metas {
		if filter == nil {
			total += int64(meta.features)
			continue
		}
		zone, err := decodeZone(meta.zone)
		if err != nil {
			return 0, err
		}
		if !mayMatch(filter, zone) {
			continue
		}
		keep, err := l.rowMask(ctx, meta, filter, meta.features)
		if err != nil {
			return 0, err
		}
		for _, ok := range keep {
			if ok {
				total++
			}
		}
	}
	return total, nil
}

func (l *Layer) catalogOf(ctx context.Context, coll Collection, level int, fresh bool) ([]chunkMeta, error) {
	if fresh {
		key := catalogKey{collection: coll.Name, uid: coll.UID, version: coll.Version, level: level, cell: 0}
		metas, err := l.loadCatalog(ctx, coll.Name, level)
		if err != nil {
			return nil, err
		}
		l.catalogs.Add(key, metas)
		return metas, nil
	}
	return l.catalog(ctx, coll, level)
}

func (l *Layer) catalog(ctx context.Context, coll Collection, level int) ([]chunkMeta, error) {
	key := catalogKey{collection: coll.Name, uid: coll.UID, version: coll.Version, level: level, cell: 0}
	if metas, ok := l.catalogs.Get(key); ok {
		return metas, nil
	}
	metas, err := l.loadCatalog(ctx, coll.Name, level)
	if err != nil {
		return nil, err
	}
	l.catalogs.Add(key, metas)
	return metas, nil
}

func (l *Layer) loadCatalog(ctx context.Context, name string, level int) ([]chunkMeta, error) {
	rows, err := l.pool.Query(ctx, `select cell, file_id, col_file, fence_file, features, minx, miny, maxx, maxy, first_key, first_id, last_key, last_id, zone from pfgb.chunks where collection = $1 and level = $2 order by first_key, first_id collate "C"`, name, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []chunkMeta
	for rows.Next() {
		m, err := scanChunkMeta(rows)
		if err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func scanChunkMeta(rows pgx.Rows) (chunkMeta, error) {
	var m chunkMeta
	var cell, firstKey, lastKey int64
	if err := rows.Scan(&cell, &m.fileID, &m.colFile, &m.fenceFile, &m.features, &m.bounds[0], &m.bounds[1], &m.bounds[2], &m.bounds[3], &firstKey, &m.firstID, &lastKey, &m.lastID, &m.zone); err != nil {
		return m, err
	}
	m.firstKey = spatial.FromOrdered(firstKey)
	m.lastKey = spatial.FromOrdered(lastKey)
	return m, nil
}

func (l *Layer) loadCatalogCells(ctx context.Context, name string, level int, cells []uint64) (map[uint64][]chunkMeta, error) {
	rows, err := l.pool.Query(ctx, `select cell, file_id, col_file, fence_file, features, minx, miny, maxx, maxy, first_key, first_id, last_key, last_id, zone from pfgb.chunks where collection = $1 and level = $2 and cell = any($3) order by first_key, first_id collate "C"`, name, level, orderedCells(cells))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCell := make(map[uint64][]chunkMeta, len(cells))
	for rows.Next() {
		var cell int64
		var m chunkMeta
		var firstKey, lastKey int64
		if err := rows.Scan(&cell, &m.fileID, &m.colFile, &m.fenceFile, &m.features, &m.bounds[0], &m.bounds[1], &m.bounds[2], &m.bounds[3], &firstKey, &m.firstID, &lastKey, &m.lastID, &m.zone); err != nil {
			return nil, err
		}
		m.firstKey = spatial.FromOrdered(firstKey)
		m.lastKey = spatial.FromOrdered(lastKey)
		key := spatial.FromOrdered(cell)
		byCell[key] = append(byCell[key], m)
	}
	return byCell, rows.Err()
}

func catalogCoverLevel(level int) int {
	if level == detailLevel {
		return spatial.PartitionLevel
	}
	return level
}

func (l *Layer) catalogFor(ctx context.Context, coll Collection, level int, cells []uint64, fresh bool) ([]chunkMeta, error) {
	if len(cells) == 0 || len(cells) > maxCatalogCells {
		return l.catalogOf(ctx, coll, level, fresh)
	}
	var out []chunkMeta
	var missing []uint64
	for _, cell := range cells {
		key := catalogKey{collection: coll.Name, uid: coll.UID, version: coll.Version, level: level, cell: cell}
		if !fresh {
			if metas, ok := l.catalogs.Get(key); ok {
				out = append(out, metas...)
				continue
			}
		}
		missing = append(missing, cell)
	}
	if len(missing) > 0 {
		byCell, err := l.loadCatalogCells(ctx, coll.Name, level, missing)
		if err != nil {
			return nil, err
		}
		for _, cell := range missing {
			metas := byCell[cell]
			key := catalogKey{collection: coll.Name, uid: coll.UID, version: coll.Version, level: level, cell: cell}
			l.catalogs.Add(key, metas)
			out = append(out, metas...)
		}
	}
	slices.SortFunc(out, func(a, b chunkMeta) int {
		return compareKeyID(a.firstKey, a.firstID, b.firstKey, b.firstID)
	})
	return out, nil
}

func catalogSize(metas []chunkMeta) int64 {
	var n int64
	for _, m := range metas {
		n += int64(128 + len(m.zone) + len(m.lastID))
	}
	return n
}

func (l *Layer) matches(ctx context.Context, idColumn string, meta chunkMeta, q Query, limit int) ([]Hit, error) {
	if limit <= 0 {
		return nil, nil
	}
	if meta.fenceFile == 0 {
		return l.matchesLegacy(ctx, idColumn, meta, q)
	}
	cell, err := l.cellFile(ctx, meta.fileID, meta.fenceFile)
	if err != nil {
		return nil, err
	}
	reader, err := l.blocks.Open(ctx, meta.fileID)
	if err != nil {
		return nil, stale(err)
	}
	start := 0
	if q.Cursor != nil {
		start = cell.fence.StartRow(q.Cursor.Key, q.Cursor.ID)
	}
	var candidates []fgb.Row
	if q.BBox != nil {
		candidates = cell.layout.Search(q.BBox[0], q.BBox[1], q.BBox[2], q.BBox[3])
		if start > 0 {
			candidates = slices.DeleteFunc(candidates, func(row fgb.Row) bool { return row.Index < start })
		}
	} else {
		rows := cell.layout.Rows()
		if start >= len(rows) {
			return nil, nil
		}
		candidates = rows[start:]
	}
	if q.Filter != nil {
		keep, err := l.rowMask(ctx, meta, q.Filter, meta.features)
		if err != nil {
			return nil, err
		}
		matched := make([]fgb.Row, 0, min(len(candidates), limit+fgb.FenceEvery))
		for _, row := range candidates {
			if row.Index < len(keep) && keep[row.Index] {
				matched = append(matched, row)
			}
		}
		candidates = matched
	}
	if len(candidates) > limit+2*fgb.FenceEvery {
		candidates = candidates[:limit+2*fgb.FenceEvery]
	}
	features, err := cell.layout.Features(reader, candidates)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, min(len(features), limit))
	for _, f := range features {
		bounds, err := featureBounds(cell.chunk, f)
		if err != nil {
			return nil, err
		}
		id, err := storedID(f, cell.cols, idColumn)
		if err != nil {
			return nil, err
		}
		key := spatial.Key(bounds)
		if q.Cursor != nil && compareKeyID(key, id, q.Cursor.Key, q.Cursor.ID) <= 0 {
			continue
		}
		if q.BBox != nil && !intersects(bounds, q.BBox) {
			continue
		}
		hits = append(hits, Hit{Key: key, ID: id, Chunk: cell.chunk, Feature: f})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

func (l *Layer) matchesLegacy(ctx context.Context, idColumn string, meta chunkMeta, q Query) ([]Hit, error) {
	data, err := l.blocks.ReadAll(ctx, meta.fileID)
	if err != nil {
		return nil, stale(err)
	}
	c, err := chunk.Open(data)
	if err != nil {
		return nil, err
	}
	var features []*flattypes.Feature
	if q.BBox != nil && q.Filter == nil {
		features, err = c.Search(q.BBox[0], q.BBox[1], q.BBox[2], q.BBox[3])
	} else {
		features, err = c.SearchAll()
	}
	if err != nil {
		return nil, err
	}
	cols := c.Columns()
	all := make([]located, 0, len(features))
	for _, f := range features {
		bounds, err := featureBounds(c, f)
		if err != nil {
			return nil, err
		}
		id, err := storedID(f, cols, idColumn)
		if err != nil {
			return nil, err
		}
		all = append(all, located{hit: Hit{Key: spatial.Key(bounds), ID: id, Chunk: c, Feature: f}, bounds: bounds})
	}
	slices.SortFunc(all, func(a, b located) int { return compareKeyID(a.hit.Key, a.hit.ID, b.hit.Key, b.hit.ID) })
	var keep []bool
	if q.Filter != nil {
		keep, err = l.rowMask(ctx, meta, q.Filter, len(all))
		if err != nil {
			return nil, err
		}
	}
	var hits []Hit
	for i, item := range all {
		if keep != nil && !keep[i] {
			continue
		}
		if keep != nil && q.BBox != nil && !intersects(item.bounds, q.BBox) {
			continue
		}
		if q.Cursor != nil && compareKeyID(item.hit.Key, item.hit.ID, q.Cursor.Key, q.Cursor.ID) <= 0 {
			continue
		}
		hits = append(hits, item.hit)
	}
	return hits, nil
}

func (l *Layer) rowMask(ctx context.Context, meta chunkMeta, filter cql.Expr, rows int) ([]bool, error) {
	if meta.colFile == 0 {
		return nil, errors.New("layer: chunk has no column file; reload the collection")
	}
	fileReader, err := l.blocks.Open(ctx, meta.colFile)
	if err != nil {
		return nil, stale(err)
	}
	if fileReader.Size() <= 1<<20 {
		data, err := l.blocks.ReadAll(ctx, meta.colFile)
		if err != nil {
			return nil, stale(err)
		}
		if bytes.HasPrefix(data, []byte(parquetMagic)) {
			return parquetMask(data, filter, rows)
		}
		return legacyColumnMask(data, filter, rows)
	}
	head := make([]byte, 4)
	if _, err := fileReader.ReadAt(head, 0); err != nil {
		return nil, stale(err)
	}
	if bytes.HasPrefix(head, []byte(parquetMagic)) {
		return parquetMaskAt(fileReader, fileReader.Size(), filter, rows)
	}
	data, err := l.blocks.ReadAll(ctx, meta.colFile)
	if err != nil {
		return nil, stale(err)
	}
	return legacyColumnMask(data, filter, rows)
}

func legacyColumnMask(data []byte, filter cql.Expr, rows int) ([]bool, error) {
	reader, ok := chunk.OpenAttrs(data)
	if !ok {
		return nil, errors.New("layer: column file unreadable")
	}
	file := reader.File()
	if file.Rows != rows {
		return nil, fmt.Errorf("layer: column file has %d rows, chunk has %d", file.Rows, rows)
	}
	props := make([]map[string]any, rows)
	for _, name := range cql.Properties(filter) {
		if _, ok := file.Column(name); !ok {
			continue
		}
		values, err := reader.Values(name)
		if err != nil {
			return nil, err
		}
		for i := range props {
			value := values.Value(i)
			if value == nil {
				continue
			}
			if props[i] == nil {
				props[i] = map[string]any{}
			}
			props[i][name] = value
		}
	}
	columns := file.ColumnTypes()
	keep := make([]bool, rows)
	for i := range keep {
		keep[i] = cql.Match(filter, props[i], columns)
	}
	return keep, nil
}

func featureBounds(c *chunk.Chunk, f *flattypes.Feature) ([4]float64, error) {
	shape, err := chunk.ParseFlat(c.Header(), f)
	if err != nil {
		return [4]float64{}, err
	}
	if shape == nil {
		return [4]float64{}, errors.New("layer: stored feature has no geometry")
	}
	bounds, ok := shape.Bound()
	if !ok {
		return [4]float64{}, errors.New("layer: stored feature has no extent")
	}
	return bounds, nil
}

func stale(err error) error {
	if errors.Is(err, blocks.ErrNotFound) {
		return ErrStale
	}
	return err
}

func intersects(bounds [4]float64, bbox *[4]float64) bool {
	return bounds[2] >= bbox[0] && bounds[0] <= bbox[2] && bounds[3] >= bbox[1] && bounds[1] <= bbox[3]
}

func storedID(f *flattypes.Feature, cols []chunk.Column, column string) (string, error) {
	value, ok := chunk.DecodeProperties(f.PropertiesBytes(), cols)[column]
	if !ok || value == nil {
		return "", fmt.Errorf("layer: stored feature lacks id %q", column)
	}
	text, ok := stats.CanonicalText(value)
	if !ok || text == "" {
		return "", fmt.Errorf("layer: stored id %q is unusable", column)
	}
	return text, nil
}
