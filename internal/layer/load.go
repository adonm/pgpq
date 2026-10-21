package layer

import (
	"cmp"
	"context"
	"errors"
	"math"
	"runtime"
	"slices"
	"sync"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
	"github.com/adonm/pgpq/internal/store"
)

const (
	overviewSampleCap = 2048
	loadWorkers       = 2
)

var coarseSteps = []struct {
	level int
	from  int
}{{6, 0}, {4, 6}, {2, 4}}

type entry struct {
	key    uint64
	id     string
	cell   uint64
	index  int
	bounds [4]float64
}

type cellRow struct {
	cell     uint64
	level    int
	features int
	bytes    int64
	cellID   int64
	bounds   [4]float64
	firstKey uint64
	firstID  string
	lastKey  uint64
	lastID   string
	zone     string
	data     []byte
	items    []geo.Feature
}

type LoadResult struct {
	Collection string `json:"collection"`
	Features   int    `json:"features"`
	Cells      int    `json:"cells"`
	Partitions int    `json:"partitions"`
}

func (l *Layer) Load(ctx context.Context, name string, ds geo.Dataset, cells []uint64, appendMode bool) (LoadResult, error) {
	if ds.IDColumn == "" || ds.Geometry == "" {
		return LoadResult{}, invalidf("the collection needs an id and geometry column")
	}
	coll, err := l.Collection(ctx, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return LoadResult{}, err
	}
	entries, err := indexFeatures(ds, cells)
	if err != nil {
		return LoadResult{}, err
	}
	groups := groupByCell(entries)
	detail := make(map[uint64][]geo.Feature, len(groups))
	for cell, group := range groups {
		features := make([]geo.Feature, len(group))
		for i, entry := range group {
			features[i] = ds.Features[entry.index]
			features[i].Key = entry.key
			features[i].ID = entry.id
		}
		if appendMode && coll.UID != 0 {
			old, err := l.existingFeatures(ctx, coll, 0, cell)
			if err != nil {
				return LoadResult{}, err
			}
			features = mergeFeatures(old, features)
		}
		slices.SortFunc(features, func(a, b geo.Feature) int { return compareKeyID(a.Key, a.ID, b.Key, b.ID) })
		detail[cell] = features
	}
	rows, err := l.writeCells(ctx, ds, detail, 0)
	if err != nil {
		return LoadResult{}, err
	}
	byLevel := map[int]map[uint64]cellRow{0: rowMap(rows)}
	touched := make([]uint64, 0, len(detail))
	for cell := range detail {
		touched = append(touched, cell)
	}
	whole := cells == nil && !appendMode
	for _, step := range coarseSteps {
		parents := ancestors(touched, step.level)
		overview := make(map[uint64][]geo.Feature, len(parents))
		for _, parent := range parents {
			children, err := l.childCells(ctx, coll, step.from, parent, byLevel[step.from], whole)
			if err != nil {
				return LoadResult{}, err
			}
			var features []geo.Feature
			for _, child := range children {
				if row, ok := byLevel[step.from][child]; ok {
					features = append(features, row.items...)
					continue
				}
				old, err := l.existingFeatures(ctx, coll, step.from, child)
				if err != nil {
					return LoadResult{}, err
				}
				features = append(features, old...)
			}
			overview[parent] = sampleFeatures(features, step.level)
		}
		next, err := l.writeCells(ctx, ds, overview, step.level)
		if err != nil {
			return LoadResult{}, err
		}
		rows = append(rows, next...)
		byLevel[step.level] = rowMap(next)
	}
	return l.commit(ctx, name, ds, cells, rows, detail, whole)
}

func indexFeatures(ds geo.Dataset, cells []uint64) ([]entry, error) {
	listed := make(map[uint64]bool, len(cells))
	for _, cell := range cells {
		listed[cell] = true
	}
	seen := make(map[string]struct{}, len(ds.Features))
	out := make([]entry, 0, len(ds.Features))
	for i := range ds.Features {
		bounds := ds.Features[i].Bounds
		if !finite(bounds) {
			return nil, invalidf("feature %d has no usable geometry", i)
		}
		key := spatial.Key(bounds)
		cell := spatial.Partition(key)
		if cells != nil && !listed[cell] {
			return nil, invalidf("feature %d lies outside the listed cells", i)
		}
		if _, dup := seen[ds.Features[i].ID]; dup {
			return nil, invalidf("duplicate id %q", ds.Features[i].ID)
		}
		seen[ds.Features[i].ID] = struct{}{}
		out = append(out, entry{key: key, id: ds.Features[i].ID, cell: cell, index: i, bounds: bounds})
	}
	return out, nil
}

func finite(bounds [4]float64) bool {
	for _, value := range bounds {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func groupByCell(entries []entry) map[uint64][]entry {
	groups := make(map[uint64][]entry)
	for _, e := range entries {
		groups[e.cell] = append(groups[e.cell], e)
	}
	for cell := range groups {
		slices.SortFunc(groups[cell], func(a, b entry) int {
			return compareKeyID(a.key, a.id, b.key, b.id)
		})
	}
	return groups
}

func (l *Layer) existingFeatures(ctx context.Context, coll Collection, level int, cell uint64) ([]geo.Feature, error) {
	metas, err := l.catalogFor(ctx, coll, level, []uint64{cell})
	if err != nil || len(metas) == 0 {
		return nil, err
	}
	file, err := l.cellFile(ctx, coll, metas[0])
	if err != nil {
		return nil, err
	}
	groups := make([]int, file.GroupCount())
	for i := range groups {
		groups[i] = i
	}
	return file.RowGroups(groups, coll.IDColumn)
}

func (l *Layer) childCells(ctx context.Context, coll Collection, level int, parent uint64, fresh map[uint64]cellRow, whole bool) ([]uint64, error) {
	seen := map[uint64]bool{}
	var out []uint64
	for cell := range fresh {
		lo, hi := spatial.Range(parent)
		if cell >= lo && cell <= hi {
			seen[cell] = true
			out = append(out, cell)
		}
	}
	if !whole && coll.UID != 0 {
		lo, hi := spatial.Range(parent)
		rows, err := l.pool.Query(ctx, `select cell from pgpq.cells where collection_uid = $1 and level = $2 and cell between $3 and $4`,
			coll.UID, level, spatial.ToOrdered(lo), spatial.ToOrdered(hi))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cell int64
			if err := rows.Scan(&cell); err != nil {
				rows.Close()
				return nil, err
			}
			id := spatial.FromOrdered(cell)
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(out)
	return out, nil
}

func rowMap(rows []cellRow) map[uint64]cellRow {
	out := make(map[uint64]cellRow, len(rows))
	for _, row := range rows {
		out[row.cell] = row
	}
	return out
}

func mergeFeatures(old, incoming []geo.Feature) []geo.Feature {
	merged := make(map[string]geo.Feature, len(old)+len(incoming))
	for _, feature := range old {
		merged[feature.ID] = feature
	}
	for _, feature := range incoming {
		merged[feature.ID] = feature
	}
	out := make([]geo.Feature, 0, len(merged))
	for _, feature := range merged {
		out = append(out, feature)
	}
	return out
}

func (l *Layer) writeCells(ctx context.Context, ds geo.Dataset, groups map[uint64][]geo.Feature, level int) ([]cellRow, error) {
	cells := make([]uint64, 0, len(groups))
	for cell, features := range groups {
		if len(features) == 0 {
			continue
		}
		cells = append(cells, cell)
	}
	slices.Sort(cells)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		out   []cellRow
		first error
	)
	sem := make(chan struct{}, min(loadWorkers, runtime.NumCPU()))
	for _, cell := range cells {
		sem <- struct{}{}
		wg.Add(1)
		go func(cell uint64) {
			defer wg.Done()
			defer func() { <-sem }()
			row, err := l.writeCell(ds, cell, groups[cell], level)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			out = append(out, row)
		}(cell)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return out, nil
}

func (l *Layer) writeCell(ds geo.Dataset, cell uint64, features []geo.Feature, level int) (cellRow, error) {
	slices.SortFunc(features, func(a, b geo.Feature) int { return compareKeyID(a.Key, a.ID, b.Key, b.ID) })
	raw, err := geo.EncodeCell(ds, features)
	if err != nil {
		return cellRow{}, err
	}
	zone, err := encodeZone(ds.Columns, features)
	if err != nil {
		return cellRow{}, err
	}
	bounds := features[0].Bounds
	for _, feature := range features[1:] {
		bounds[0] = min(bounds[0], feature.Bounds[0])
		bounds[1] = min(bounds[1], feature.Bounds[1])
		bounds[2] = max(bounds[2], feature.Bounds[2])
		bounds[3] = max(bounds[3], feature.Bounds[3])
	}
	first, last := features[0], features[len(features)-1]
	return cellRow{
		cell:     cell,
		level:    level,
		features: len(features),
		bytes:    int64(len(raw)),
		bounds:   bounds,
		firstKey: first.Key,
		firstID:  first.ID,
		lastKey:  last.Key,
		lastID:   last.ID,
		zone:     zone,
		data:     raw,
		items:    features,
	}, nil
}

func sampleFeatures(features []geo.Feature, level int) []geo.Feature {
	features = slices.DeleteFunc(features, func(feature geo.Feature) bool {
		return sampleTooSmall(feature.Bounds, level)
	})
	slices.SortFunc(features, func(a, b geo.Feature) int {
		if c := cmp.Compare(idHash(a.ID), idHash(b.ID)); c != 0 {
			return c
		}
		return compareKeyID(a.Key, a.ID, b.Key, b.ID)
	})
	features = features[:min(len(features), overviewSampleCap)]
	slices.SortFunc(features, func(a, b geo.Feature) int {
		return compareKeyID(a.Key, a.ID, b.Key, b.ID)
	})
	return features
}

func samplePixelMeters(level int) float64 {
	var zoom float64
	switch level {
	case 2:
		zoom = 6
	case 4:
		zoom = 8
	case 6:
		zoom = 10
	default:
		return 0
	}
	return 2 * math.Pi * 6378137 * math.Cos(48*math.Pi/180) / (256 * math.Exp2(zoom))
}

func sampleTooSmall(bounds [4]float64, level int) bool {
	limit := samplePixelMeters(level)
	if limit == 0 {
		return false
	}
	lat := (bounds[1] + bounds[3]) / 2 * math.Pi / 180
	width := (bounds[2] - bounds[0]) * 111320 * math.Cos(lat)
	height := (bounds[3] - bounds[1]) * 111320
	size := math.Max(width, height)
	return size > 0 && size < limit
}

func compareKeyID(k1 uint64, id1 string, k2 uint64, id2 string) int {
	switch {
	case k1 < k2:
		return -1
	case k1 > k2:
		return 1
	case id1 < id2:
		return -1
	case id1 > id2:
		return 1
	}
	return 0
}
