package layer

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/spatial"
	"github.com/adonm/postfgb/internal/stats"
)

const (
	targetChunkBytes  = 2 << 20
	maxChunkFeatures  = 50000
	maxChunkBytes     = 8 << 20
	detailLevel       = 0
	overviewSampleCap = 2048
	sampleLatitude    = 48.0
	metersPerDegree   = 111320.0
)

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
	return 2 * math.Pi * 6378137 * math.Cos(sampleLatitude*math.Pi/180) / (256 * math.Exp2(zoom))
}

func sampleTooSmall(bounds [4]float64, level int) bool {
	limit := samplePixelMeters(level)
	if limit == 0 {
		return false
	}
	lat := (bounds[1] + bounds[3]) / 2 * math.Pi / 180
	width := (bounds[2] - bounds[0]) * metersPerDegree * math.Cos(lat)
	height := (bounds[3] - bounds[1]) * metersPerDegree
	size := math.Max(width, height)
	return size > 0 && size < limit
}

// coarseStep rebuilds one overview level from the level below it.
type coarseStep struct {
	level int
	from  int
}

var (
	overviewLevels = []int{}
	coarseSteps    = []coarseStep{{level: 6, from: detailLevel}, {level: 4, from: 6}}
	deferredLevel  = 2
	servedLevels   = []int{2, 4, 6}
)

type blobPut func(ctx context.Context, collection, kind string, data []byte) (int64, error)

type entry struct {
	key    uint64
	id     string
	hash   uint64
	cell   uint64
	index  int
	bounds [4]float64
}

type chunkRow struct {
	cell    uint64
	level   int
	seq     int
	first   entry
	last    entry
	bounds  [4]float64
	rows    int
	fileID  int64
	colFile int64
	zone    string
}

type LoadResult struct {
	Collection string `json:"collection"`
	Features   int    `json:"features"`
	Chunks     int    `json:"chunks"`
	Partitions int    `json:"partitions"`
}

func (l *Layer) Load(ctx context.Context, name string, ds ingest.Dataset, cells []uint64) (LoadResult, error) {
	if ds.IDColumn == "" {
		return LoadResult{}, invalidf("the collection needs an id column")
	}
	if cells == nil && len(ds.Features) == 0 {
		return LoadResult{}, invalidf("a whole-collection load needs features")
	}
	entries, err := indexFeatures(ds, cells)
	if err != nil {
		return LoadResult{}, err
	}
	groups := groupByCell(entries)
	chunks, err := l.writeCells(ctx, l.blocks.Put, name, ds, groups, detailLevel)
	if err != nil {
		return LoadResult{}, err
	}
	for _, level := range overviewLevels {
		rows, err := l.writeCells(ctx, l.blocks.Put, name, ds, groupByCell(sampleOverview(entries, level)), level)
		if err != nil {
			return LoadResult{}, err
		}
		chunks = append(chunks, rows...)
	}
	return l.commit(ctx, name, ds, cells, chunks, entries)
}

func sampleOverview(entries []entry, level int) []entry {
	groups := map[uint64][]entry{}
	for _, e := range entries {
		if sampleTooSmall(e.bounds, level) {
			continue
		}
		cell := spatial.Level(e.key, level)
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
		out = append(out, group[:min(len(group), overviewSampleCap)]...)
	}
	return out
}

func indexFeatures(ds ingest.Dataset, cells []uint64) ([]entry, error) {
	listed := make(map[uint64]bool, len(cells))
	for _, cell := range cells {
		listed[cell] = true
	}
	seen := make(map[string]struct{}, len(ds.Features))
	out := make([]entry, 0, len(ds.Features))
	for i := range ds.Features {
		bounds, ok := chunk.FeatureBounds(&ds.Features[i])
		if !ok || !finite(bounds) {
			return nil, invalidf("feature %d has no usable geometry", i)
		}
		key := spatial.Key(bounds)
		cell := spatial.Partition(key)
		if cells != nil && !listed[cell] {
			return nil, invalidf("feature %d lies outside the listed cells", i)
		}
		id, err := canonicalID(ds.Features[i], ds.IDColumn)
		if err != nil {
			return nil, invalidf("feature %d: %v", i, err)
		}
		if _, dup := seen[id]; dup {
			return nil, invalidf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
		out = append(out, entry{key: key, id: id, hash: idHash(id), cell: cell, index: i, bounds: bounds})
	}
	return out, nil
}

func finite(bounds [4]float64) bool {
	for _, v := range bounds {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func canonicalID(f chunk.InputFeature, column string) (string, error) {
	raw, ok := f.Properties[column]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return "", errors.New("missing id")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	text, ok := stats.CanonicalText(value)
	if !ok || text == "" {
		return "", errors.New("unusable id value")
	}
	return text, nil
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

func (l *Layer) writeCells(ctx context.Context, put blobPut, name string, ds ingest.Dataset, groups map[uint64][]entry, level int) ([]chunkRow, error) {
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
		out   []chunkRow
		first error
	)
	sem := make(chan struct{}, runtime.NumCPU())
	for _, cell := range cells {
		sem <- struct{}{}
		wg.Add(1)
		go func(cell uint64) {
			defer wg.Done()
			defer func() { <-sem }()
			rows, err := l.writeCell(ctx, put, name, ds, cell, groups[cell], level)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			out = append(out, rows...)
		}(cell)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return out, nil
}

func (l *Layer) writeCell(ctx context.Context, put blobPut, name string, ds ingest.Dataset, cell uint64, entries []entry, level int) ([]chunkRow, error) {
	var rows []chunkRow
	start, estimated := 0, 0
	for i := range entries {
		estimated += estimate(ds.Features[entries[i].index])
		count := i - start + 1
		last := i == len(entries)-1
		if !last && estimated < targetChunkBytes && count < maxChunkFeatures {
			continue
		}
		cut, err := l.writeRange(ctx, put, name, ds, cell, entries[start:i+1], level)
		if err != nil {
			return nil, err
		}
		for _, row := range cut {
			row.seq = len(rows)
			rows = append(rows, row)
		}
		start, estimated = i+1, 0
	}
	return rows, nil
}

func (l *Layer) writeRange(ctx context.Context, put blobPut, name string, ds ingest.Dataset, cell uint64, entries []entry, level int) ([]chunkRow, error) {
	batch := make([]chunk.InputFeature, len(entries))
	for i, e := range entries {
		batch[i] = ds.Features[e.index]
	}
	data, err := chunk.EncodeCols(name, ds.Columns, batch)
	if err != nil {
		return nil, err
	}
	if len(data) > maxChunkBytes && len(entries) > 1 {
		mid := len(entries) / 2
		left, err := l.writeRange(ctx, put, name, ds, cell, entries[:mid], level)
		if err != nil {
			return nil, err
		}
		right, err := l.writeRange(ctx, put, name, ds, cell, entries[mid:], level)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	props := make([]map[string]json.RawMessage, len(batch))
	for i := range batch {
		props[i] = batch[i].Properties
	}
	blob, summaries, err := encodeParquetColumns(ds.Columns, props)
	if err != nil {
		return nil, err
	}
	zone, err := encodeZoneSummaries(summaries)
	if err != nil {
		return nil, err
	}
	fileID, err := put(ctx, name, "fgb", chunk.Compress(data))
	if err != nil {
		return nil, err
	}
	colFile, err := put(ctx, name, "cols", blob)
	if err != nil {
		return nil, err
	}
	return []chunkRow{{
		cell:    cell,
		level:   level,
		first:   entries[0],
		last:    entries[len(entries)-1],
		bounds:  unionBounds(entries),
		rows:    len(entries),
		fileID:  fileID,
		colFile: colFile,
		zone:    zone,
	}}, nil
}

func estimate(f chunk.InputFeature) int {
	n := 52
	if f.Parsed != nil {
		n += f.Parsed.Estimate()
	}
	for _, value := range f.Properties {
		n += len(value) + 3
	}
	return n
}

func unionBounds(entries []entry) [4]float64 {
	b := entries[0].bounds
	for _, e := range entries[1:] {
		b[0] = math.Min(b[0], e.bounds[0])
		b[1] = math.Min(b[1], e.bounds[1])
		b[2] = math.Max(b[2], e.bounds[2])
		b[3] = math.Max(b[3], e.bounds[3])
	}
	return b
}

func compareKeyID(k1 uint64, id1 string, k2 uint64, id2 string) int {
	switch {
	case k1 < k2:
		return -1
	case k1 > k2:
		return 1
	}
	return strings.Compare(id1, id2)
}
