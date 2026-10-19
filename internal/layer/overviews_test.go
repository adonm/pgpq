package layer

import (
	"cmp"
	"errors"
	"slices"
	"testing"

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
		for _, h := range hits[:min(len(hits), overviewCap)] {
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
	detail := readAll(t, l, name, Query{Limit: 64})
	for _, level := range []int{2, 6, 8} {
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
	detail := readAll(t, l, name, Query{Limit: 64})
	for _, level := range []int{2, 6} {
		checkTopPerCell(t, l, name, level, detail)
	}
}

func TestLoadIfUnchangedRejectsAStaleBase(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_base"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := cityFeatures("paris", paris, 40, 71)
	cells := cellsOf(t, features)
	ds := datasetOf(t, name, testColumns, features)
	first, err := l.Load(ctx, name, ds, cells)
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.LoadIfUnchanged(ctx, name, ds, cells, first.Snapshot)
	if err != nil {
		t.Fatalf("load on the base it read: %v", err)
	}
	if _, err := l.LoadIfUnchanged(ctx, name, ds, cells, first.Snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("load on a stale base: %v", err)
	}
	if _, err := l.LoadIfUnchanged(ctx, name, ds, cells, second.Snapshot); err != nil {
		t.Fatalf("load on the newer base: %v", err)
	}
}
