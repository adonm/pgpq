package layer

import (
	"context"
	"slices"

	"github.com/adonm/pgpq/internal/cql"
	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
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
	Feature geo.Feature
}

type Page struct {
	Collection string
	IDColumn   string
	Geometry   string
	Columns    []geo.Column
	Hits       []Hit
	Next       *Cursor
}

const maxCatalogCells = 256

func (l *Layer) Items(ctx context.Context, name string, q Query) (Page, error) {
	if q.Limit <= 0 {
		return Page{}, invalidf("limit must be positive")
	}
	if q.Level != 0 && q.Level != 2 && q.Level != 4 && q.Level != 6 {
		return Page{}, invalidf("overview level %d is not served", q.Level)
	}
	if q.Level != 0 && q.Filter != nil {
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
		cells = spatial.Cover(*q.BBox, coverLevel(q.Level))
	}
	metas, err := l.catalogFor(ctx, coll, q.Level, cells)
	if err != nil {
		return Page{}, err
	}
	page := Page{Collection: name, IDColumn: coll.IDColumn, Geometry: coll.Geometry, Columns: coll.Columns}
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
		file, err := l.cellFile(ctx, coll, meta)
		if err != nil {
			return Page{}, err
		}
		groups := selectGroups(file, q)
		err = file.Scan(groups, coll.IDColumn, func(feature geo.Feature) bool {
			if q.Cursor != nil && compareKeyID(feature.Key, feature.ID, q.Cursor.Key, q.Cursor.ID) <= 0 {
				return true
			}
			if q.BBox != nil && !intersects(feature.Bounds, q.BBox) {
				return true
			}
			if q.Filter != nil && !cql.Match(q.Filter, feature.Props, columnTypes(coll.Columns)) {
				return true
			}
			page.Hits = append(page.Hits, Hit{Key: feature.Key, Feature: feature})
			return len(page.Hits) < q.Limit
		})
		if err != nil {
			return Page{}, err
		}
	}
	if len(page.Hits) == q.Limit {
		last := page.Hits[len(page.Hits)-1]
		page.Next = &Cursor{UID: coll.UID, Level: q.Level, Key: last.Key, ID: last.Feature.ID}
	}
	return page, nil
}

func (l *Layer) Count(ctx context.Context, name string, filter cql.Expr) (int64, error) {
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return 0, err
	}
	if filter == nil {
		var total int64
		err := l.pool.QueryRow(ctx, `select coalesce(sum(features), 0) from pgpq.cells where collection_uid = $1 and level = 0`, coll.UID).Scan(&total)
		return total, err
	}
	metas, err := l.catalog(ctx, coll, 0)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, meta := range metas {
		zone, err := decodeZone(meta.zone)
		if err != nil {
			return 0, err
		}
		if !mayMatch(filter, zone) {
			continue
		}
		file, err := l.cellFile(ctx, coll, meta)
		if err != nil {
			return 0, err
		}
		groups := selectGroups(file, Query{Filter: filter, Level: 0, Limit: 1})
		err = file.Scan(groups, coll.IDColumn, func(feature geo.Feature) bool {
			if cql.Match(filter, feature.Props, columnTypes(coll.Columns)) {
				total++
			}
			return true
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (l *Layer) catalog(ctx context.Context, coll Collection, level int) ([]cellMeta, error) {
	key := catalogKey{uid: coll.UID, version: coll.Version, level: level}
	if metas, ok := l.catalogs.Get(key); ok {
		return metas, nil
	}
	return l.catalogQuery(ctx, coll, level, nil)
}

func (l *Layer) catalogFor(ctx context.Context, coll Collection, level int, cells []uint64) ([]cellMeta, error) {
	if len(cells) == 0 || len(cells) > maxCatalogCells {
		return l.catalog(ctx, coll, level)
	}
	var out []cellMeta
	var missing []uint64
	for _, cell := range cells {
		key := catalogKey{uid: coll.UID, version: coll.Version, level: level, cell: cell}
		if metas, ok := l.catalogs.Get(key); ok {
			out = append(out, metas...)
			continue
		}
		missing = append(missing, cell)
	}
	if len(missing) > 0 {
		metas, err := l.catalogQuery(ctx, coll, level, missing)
		if err != nil {
			return nil, err
		}
		for _, meta := range metas {
			key := catalogKey{uid: coll.UID, version: coll.Version, level: level, cell: meta.cell}
			l.catalogs.Add(key, []cellMeta{meta})
			out = append(out, meta)
		}
	}
	slices.SortFunc(out, func(a, b cellMeta) int {
		return compareKeyID(a.firstKey, a.firstID, b.firstKey, b.firstID)
	})
	return out, nil
}

func (l *Layer) catalogQuery(ctx context.Context, coll Collection, level int, cells []uint64) ([]cellMeta, error) {
	query := `select cell, features, bytes, minx, miny, maxx, maxy, first_key, first_id, last_key, last_id, zone from pgpq.cells where collection_uid = $1 and level = $2`
	args := []any{coll.UID, level}
	if cells != nil {
		query += ` and cell = any($3)`
		args = append(args, orderedCells(cells))
	}
	query += ` order by first_key, first_id collate "C"`
	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []cellMeta
	for rows.Next() {
		var meta cellMeta
		var cell, firstKey, lastKey int64
		if err := rows.Scan(&cell, &meta.features, &meta.bytes, &meta.bounds[0], &meta.bounds[1], &meta.bounds[2], &meta.bounds[3], &firstKey, &meta.firstID, &lastKey, &meta.lastID, &meta.zone); err != nil {
			return nil, err
		}
		meta.level = level
		meta.cell = spatial.FromOrdered(cell)
		meta.firstKey = spatial.FromOrdered(firstKey)
		meta.lastKey = spatial.FromOrdered(lastKey)
		metas = append(metas, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cells == nil {
		key := catalogKey{uid: coll.UID, version: coll.Version, level: level}
		l.catalogs.Add(key, metas)
	}
	return metas, nil
}

func selectGroups(file *geo.File, q Query) []int {
	groups := make([]int, 0, file.GroupCount())
	cursor := int64(0)
	if q.Cursor != nil {
		cursor = spatial.ToOrdered(q.Cursor.Key)
	}
	for i := 0; i < file.GroupCount(); i++ {
		if q.BBox != nil {
			if box, ok := file.GroupBBox(i); ok && !intersects(box, q.BBox) {
				continue
			}
		}
		if q.Filter != nil && !mayMatch(q.Filter, groupZone(file, i)) {
			continue
		}
		if q.Cursor != nil {
			_, hi, ok := file.KeyRange(i)
			if ok && hi < cursor {
				continue
			}
		}
		groups = append(groups, i)
	}
	return groups
}

func groupZone(file *geo.File, group int) map[string]zoneColumn {
	stats := file.GroupStats(group)
	zone := make(map[string]zoneColumn, len(stats))
	for name, stat := range stats {
		zone[name] = zoneColumn{Name: name, Nulls: stat.Nulls, Min: stat.Min, Max: stat.Max}
	}
	return zone
}

func columnTypes(cols []geo.Column) map[string]cql.Type {
	types := make(map[string]cql.Type, len(cols))
	for _, col := range cols {
		types[col.Name] = col.Type
	}
	return types
}

func coverLevel(level int) int {
	if level == 0 {
		return spatial.PartitionLevel
	}
	return level
}

func intersects(bounds [4]float64, bbox *[4]float64) bool {
	return bounds[2] >= bbox[0] && bounds[0] <= bbox[2] && bounds[3] >= bbox[1] && bounds[1] <= bbox[3]
}

func orderedCells(cells []uint64) []int64 {
	out := make([]int64, len(cells))
	for i, cell := range cells {
		out[i] = spatial.ToOrdered(cell)
	}
	return out
}
