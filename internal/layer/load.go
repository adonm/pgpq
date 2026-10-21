package layer

import (
	"cmp"
	"context"
	"math"
	"runtime"
	"slices"
	"sync"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
)

const (
	overviewSampleCap = 2048
	nodeSize          = 16
	loadWorkers       = 2
)

var coarseSteps = []struct {
	level int
	from  int
}{{6, 0}, {4, 6}, {2, 4}}

type entry struct {
	key    uint64
	id     string
	hash   uint64
	cell   uint64
	index  int
	bounds [4]float64
}

type cellRow struct {
	cell     uint64
	level    int
	features int
	bounds   [4]float64
	firstKey uint64
	firstID  string
	lastKey  uint64
	lastID   string
	zone     string
	data     []byte
}

type LoadResult struct {
	Collection string `json:"collection"`
	Features   int    `json:"features"`
	Cells      int    `json:"cells"`
	Partitions int    `json:"partitions"`
}

func (l *Layer) Load(ctx context.Context, name string, ds geo.Dataset, cells []uint64) (LoadResult, error) {
	if ds.IDColumn == "" || ds.Geometry == "" {
		return LoadResult{}, invalidf("the collection needs an id and geometry column")
	}
	entries, err := indexFeatures(ds, cells)
	if err != nil {
		return LoadResult{}, err
	}
	groups := groupByCell(entries)
	rows, err := l.writeCells(ctx, name, ds, groups, 0)
	if err != nil {
		return LoadResult{}, err
	}
	for _, step := range coarseSteps {
		sampled := sampleOverview(ds, entries, step.level)
		overview, err := l.writeCells(ctx, name, ds, groupByCell(sampled), step.level)
		if err != nil {
			return LoadResult{}, err
		}
		rows = append(rows, overview...)
	}
	return l.commit(ctx, name, ds, cells, rows, groups)
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
		out = append(out, entry{key: key, id: ds.Features[i].ID, hash: idHash(ds.Features[i].ID), cell: cell, index: i, bounds: bounds})
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

func (l *Layer) writeCells(ctx context.Context, name string, ds geo.Dataset, groups map[uint64][]entry, level int) ([]cellRow, error) {
	cells := make([]uint64, 0, len(groups))
	for cell := range groups {
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
			row, err := l.writeCell(ctx, name, ds, cell, groups[cell], level)
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

func (l *Layer) writeCell(ctx context.Context, name string, ds geo.Dataset, cell uint64, entries []entry, level int) (cellRow, error) {
	features := make([]geo.Feature, len(entries))
	bounds := entries[0].bounds
	for i, e := range entries {
		features[i] = ds.Features[e.index]
		features[i].Key = e.key
		features[i].ID = e.id
		if i > 0 {
			bounds[0] = min(bounds[0], e.bounds[0])
			bounds[1] = min(bounds[1], e.bounds[1])
			bounds[2] = max(bounds[2], e.bounds[2])
			bounds[3] = max(bounds[3], e.bounds[3])
		}
	}
	raw, err := geo.EncodeCell(ds, features)
	if err != nil {
		return cellRow{}, err
	}
	zone, err := encodeZone(ds.Columns, features)
	if err != nil {
		return cellRow{}, err
	}
	last := entries[len(entries)-1]
	return cellRow{
		cell:     cell,
		level:    level,
		features: len(entries),
		bounds:   bounds,
		firstKey: entries[0].key,
		firstID:  entries[0].id,
		lastKey:  last.key,
		lastID:   last.id,
		zone:     zone,
		data:     raw,
	}, nil
}

func sampleOverview(ds geo.Dataset, entries []entry, level int) []entry {
	groups := map[uint64][]entry{}
	for _, e := range entries {
		if sampleTooSmall(e.bounds, level) {
			continue
		}
		cell := spatial.Level(e.key, level)
		e.cell = cell
		groups[cell] = append(groups[cell], e)
	}
	var out []entry
	for _, group := range groups {
		slices.SortFunc(group, func(a, b entry) int {
			if c := cmp.Compare(a.hash, b.hash); c != 0 {
				return c
			}
			return compareKeyID(a.key, a.id, b.key, b.id)
		})
		group = group[:min(len(group), overviewSampleCap)]
		slices.SortFunc(group, func(a, b entry) int {
			return compareKeyID(a.key, a.id, b.key, b.id)
		})
		out = append(out, group...)
	}
	return out
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
