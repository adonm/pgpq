package layer

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/spatial"
)

func checkTopPerCell(t *testing.T, l *Layer, name string, level int, detail []Hit) {
	t.Helper()
	world := [4]float64{-180, -90, 180, 90}
	got := readAll(t, l, name, Query{Limit: 64, Level: level, BBox: &world})
	byCell := map[uint64][]Hit{}
	for _, h := range detail {
		cell := spatial.Level(h.Key, level)
		byCell[cell] = append(byCell[cell], h)
	}
	want := map[uint64][]string{}
	for cell, hits := range byCell {
		slices.SortFunc(hits, func(a, b Hit) int { return cmp.Compare(idHash(a.ID), idHash(b.ID)) })
		for _, h := range hits[:min(len(hits), overviewSampleCap)] {
			want[cell] = append(want[cell], h.ID)
		}
		slices.Sort(want[cell])
	}
	have := map[uint64][]string{}
	for _, h := range got {
		cell := spatial.Level(h.Key, level)
		have[cell] = append(have[cell], h.ID)
	}
	for cell := range have {
		slices.Sort(have[cell])
	}
	if len(have) != len(want) {
		t.Fatalf("level %d: overview has %d cells, want %d", level, len(have), len(want))
	}
	for cell, ids := range want {
		if !slices.Equal(have[cell], ids) {
			t.Fatalf("level %d cell %x: overview has %d features, want the %d smallest hashes", level, cell, len(have[cell]), len(ids))
		}
	}
}

func TestOverviewLevelsKeepTheSmallestHashesPerCell(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_levels"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := slices.Concat(cityFeatures("paris", paris, 300, 61), cityFeatures("dhaka", dhaka, 300, 62), cityFeatures("lagos", lagos, 200, 63))
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, features), nil); err != nil {
		t.Fatal(err)
	}
	if err := l.FlushDirty(ctx); err != nil {
		t.Fatal(err)
	}
	detail := readAll(t, l, name, Query{Limit: 64})
	for _, level := range []int{2, 4, 6} {
		checkTopPerCell(t, l, name, level, detail)
	}
}

func TestReloadRebuildsCoarseLevels(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_coarse"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	first := cityFeatures("paris", paris, 300, 66)
	cells := cellsOf(t, first)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, first), cells); err != nil {
		t.Fatal(err)
	}
	second := cityFeatures("paris2", paris, 120, 67)
	cells = slices.Concat(cellsOf(t, first), cellsOf(t, second))
	slices.Sort(cells)
	cells = slices.Compact(cells)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, second), cells); err != nil {
		t.Fatal(err)
	}
	if err := l.FlushDirty(ctx); err != nil {
		t.Fatal(err)
	}
	detail := readAll(t, l, name, Query{Limit: 64})
	for _, level := range []int{2, 4, 6} {
		checkTopPerCell(t, l, name, level, detail)
	}
}

func TestL2OverviewWaitsForFlush(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_deferred"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := cityFeatures("paris", paris, 300, 71)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, features), nil); err != nil {
		t.Fatal(err)
	}
	levelCount := func(level int) int {
		t.Helper()
		var n int
		if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.chunks where collection = $1 and level = $2`, name, level).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if levelCount(6) == 0 {
		t.Fatal("level 6 was not built by the load")
	}
	if levelCount(4) == 0 {
		t.Fatal("level 4 was not built by the load")
	}
	if levelCount(8) != 0 {
		t.Fatalf("level 8 sample has %d chunks", levelCount(8))
	}
	if levelCount(2) != 0 {
		t.Fatalf("level 2 already has %d chunks before the worker", levelCount(2))
	}
	var dirty int
	if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.overview_dirty where collection = $1 and level = 2`, name).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty == 0 {
		t.Fatal("the load did not queue a level 2 overview")
	}
	world := [4]float64{-180, -90, 180, 90}
	if hits := readAll(t, l, name, Query{Limit: 64, Level: 2, BBox: &world}); len(hits) != 0 {
		t.Fatalf("level 2 served %d hits before the worker", len(hits))
	}
	if err := l.FlushDirty(ctx); err != nil {
		t.Fatal(err)
	}
	if levelCount(2) == 0 {
		t.Fatal("the worker did not build level 2")
	}
	if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.overview_dirty where collection = $1`, name).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("dirty rows after the flush = %d (%v)", dirty, err)
	}
	if hits := readAll(t, l, name, Query{Limit: 64, Level: 2, BBox: &world}); len(hits) == 0 {
		t.Fatal("level 2 is empty after the worker")
	}
}

func TestCoarseChunksDropSubpixelFeatures(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_subpixel"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	point := cityFeatures("point", paris, 1, 73)[0]
	tiny := polygonFeature("tiny", 2.3500, 48.8500, 2.3501, 48.8501)
	large := polygonFeature("large", 2.30, 48.80, 2.35, 48.85)
	features := []chunk.InputFeature{point, tiny, large}
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, features), nil); err != nil {
		t.Fatal(err)
	}
	detail := readAll(t, l, name, Query{Limit: 10})
	if len(detail) != 3 {
		t.Fatalf("detail has %d features, want 3", len(detail))
	}
	world := [4]float64{-180, -90, 180, 90}
	ids := map[string]bool{}
	for _, h := range readAll(t, l, name, Query{Limit: 10, Level: 6, BBox: &world}) {
		ids[h.ID] = true
	}
	if !ids["point-0000"] {
		t.Fatal("the point was dropped from level 6")
	}
	if !ids["large"] {
		t.Fatal("the large polygon was dropped from level 6")
	}
	if ids["tiny"] {
		t.Fatal("the subpixel polygon was kept in level 6")
	}
}

func polygonFeature(id string, minX, minY, maxX, maxY float64) chunk.InputFeature {
	coords, _ := json.Marshal([][][]float64{{
		{minX, minY}, {maxX, minY}, {maxX, maxY}, {minX, maxY}, {minX, minY},
	}})
	return chunk.InputFeature{
		Geometry: chunk.RawGeometry{Type: "Polygon", Coordinates: coords},
		Properties: map[string]json.RawMessage{
			"id":   json.RawMessage(strconv.Quote(id)),
			"name": json.RawMessage(strconv.Quote("polygon")),
		},
	}
}

func TestDirtyWorkerWaitsForQuiet(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_dirty_quiet"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := cityFeatures("paris", paris, 120, 72)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, features), nil); err != nil {
		t.Fatal(err)
	}
	if err := l.flushDirty(ctx, false, dirtyBatch); err != nil {
		t.Fatal(err)
	}
	var level int
	if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.chunks where collection = $1 and level = 2`, name).Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != 0 {
		t.Fatalf("the worker built level 2 before the quiet window: %d chunks", level)
	}
	if _, err := l.pool.Exec(ctx, `update pfgb.overview_dirty set dirty_since = now() - interval '6 seconds' where collection = $1`, name); err != nil {
		t.Fatal(err)
	}
	if err := l.flushDirty(ctx, false, dirtyBatch); err != nil {
		t.Fatal(err)
	}
	if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.chunks where collection = $1 and level = 2`, name).Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level == 0 {
		t.Fatal("the worker ignored a dirty row past the quiet window")
	}
}
