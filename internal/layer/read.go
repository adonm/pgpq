package layer

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
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
	Snapshot   int64
	Hits       []Hit
	Next       *Cursor
}

type chunkMeta struct {
	fileID   int64
	colFile  int64
	features int
	bounds   [4]float64
	zone     string
	lastKey  uint64
	lastID   string
}

type located struct {
	hit    Hit
	bounds [4]float64
}

type catalogKey struct {
	collection string
	uid        int64
	snapshot   int64
	level      int
}

func (l *Layer) Items(ctx context.Context, name string, q Query) (Page, error) {
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
	snapshot := coll.Snapshot
	metas, err := l.catalog(ctx, coll, snapshot, q.Level)
	if err != nil {
		return Page{}, err
	}
	page := Page{Collection: name, Snapshot: snapshot}
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
		hits, err := l.matches(ctx, coll.IDColumn, meta, q)
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

func (l *Layer) Count(ctx context.Context, name string, snapshot int64, filter cql.Expr) (int64, error) {
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return 0, err
	}
	metas, err := l.catalog(ctx, coll, snapshot, detailLevel)
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

func (l *Layer) catalog(ctx context.Context, coll Collection, snapshot int64, level int) ([]chunkMeta, error) {
	key := catalogKey{collection: coll.Name, uid: coll.UID, snapshot: snapshot, level: level}
	if metas, ok := l.catalogs.Get(key); ok {
		return metas, nil
	}
	metas, err := l.loadCatalog(ctx, coll.Name, snapshot, level)
	if err != nil {
		return nil, err
	}
	l.catalogs.Add(key, metas)
	return metas, nil
}

func (l *Layer) loadCatalog(ctx context.Context, name string, snapshot int64, level int) ([]chunkMeta, error) {
	rows, err := l.pool.Query(ctx, `select file_id, col_file, features, minx, miny, maxx, maxy, last_key, last_id, zone from pfgb.chunks where collection = $1 and begin_snapshot <= $2 and (end_snapshot is null or end_snapshot > $2) and level = $3 order by first_key, first_id collate "C"`, name, snapshot, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []chunkMeta
	for rows.Next() {
		var m chunkMeta
		var lastKey int64
		if err := rows.Scan(&m.fileID, &m.colFile, &m.features, &m.bounds[0], &m.bounds[1], &m.bounds[2], &m.bounds[3], &lastKey, &m.lastID, &m.zone); err != nil {
			return nil, err
		}
		m.lastKey = spatial.FromOrdered(lastKey)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func catalogSize(metas []chunkMeta) int64 {
	var n int64
	for _, m := range metas {
		n += int64(128 + len(m.zone) + len(m.lastID))
	}
	return n
}

func (l *Layer) matches(ctx context.Context, idColumn string, meta chunkMeta, q Query) ([]Hit, error) {
	data, err := l.blocks.ReadAll(ctx, meta.fileID)
	if err != nil {
		return nil, err
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
	data, err := l.blocks.ReadAll(ctx, meta.colFile)
	if err != nil {
		return nil, err
	}
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
