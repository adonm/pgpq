package layer

import (
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/spatial"
)

func TestSampleOverviewKeepsTheSmallestHashesPerCell(t *testing.T) {
	key := spatial.Key([4]float64{2.35, 48.85, 2.35, 48.85})
	var entries []entry
	for i := range overviewSampleCap + 44 {
		id := strconv.Itoa(i)
		entries = append(entries, entry{key: key, id: id, hash: idHash(id), cell: spatial.Partition(key), bounds: [4]float64{2.35, 48.85, 2.35, 48.85}})
	}
	sample := sampleOverview(entries, 6)
	if len(sample) != overviewSampleCap {
		t.Fatalf("sample holds %d, want %d", len(sample), overviewSampleCap)
	}
	hashes := make([]uint64, len(entries))
	for i, e := range entries {
		hashes[i] = e.hash
	}
	slices.Sort(hashes)
	for _, e := range sample {
		if e.hash > hashes[overviewSampleCap-1] {
			t.Fatalf("kept hash %x above the cut %x", e.hash, hashes[overviewSampleCap-1])
		}
	}
}

func TestSampleOverviewDropsSubpixelFeatures(t *testing.T) {
	point := sampleEntry("point", [4]float64{2.35, 48.85, 2.35, 48.85})
	tiny := sampleEntry("tiny", [4]float64{2.35, 48.85, 2.3501, 48.8501})
	large := sampleEntry("large", [4]float64{2.30, 48.80, 2.35, 48.85})
	ids := map[string]bool{}
	for _, e := range sampleOverview([]entry{point, tiny, large}, 6) {
		ids[e.id] = true
	}
	if !ids["point"] {
		t.Fatal("the point was dropped")
	}
	if !ids["large"] {
		t.Fatal("the large polygon was dropped")
	}
	if ids["tiny"] {
		t.Fatal("the subpixel polygon was kept")
	}
}

func sampleEntry(id string, bounds [4]float64) entry {
	key := spatial.Key(bounds)
	return entry{key: key, id: id, hash: idHash(id), cell: spatial.Partition(key), bounds: bounds}
}

func TestOverviewSamplesAndStaysOutOfLookups(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_overview"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := slices.Concat(cityFeatures("paris", paris, 600, 51), cityFeatures("dhaka", dhaka, 300, 52))
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, features), nil); err != nil {
		t.Fatal(err)
	}
	world := [4]float64{-180, -90, 180, 90}
	detail := readAll(t, l, name, Query{Limit: 64})
	overview := readAll(t, l, name, Query{Limit: 64, Level: 6, BBox: &world})
	checkOrdered(t, overview)
	detailIDs := map[string]bool{}
	population := map[uint64]int{}
	for _, h := range detail {
		detailIDs[h.ID] = true
		population[spatial.Level(h.Key, 6)]++
	}
	kept := map[uint64]int{}
	for _, h := range overview {
		if !detailIDs[h.ID] {
			t.Fatalf("overview feature %s is not a detail feature", h.ID)
		}
		kept[spatial.Level(h.Key, 6)]++
	}
	for cell, n := range population {
		if want := min(n, overviewSampleCap); kept[cell] != want {
			t.Fatalf("cell %x keeps %d features, want %d of %d", cell, kept[cell], want, n)
		}
	}
	if _, err := l.Lookup(ctx, name, detail[0].ID); err != nil {
		t.Fatalf("lookup of a detail feature with an overview present: %v", err)
	}
	paged := readAll(t, l, name, Query{Limit: 3, Level: 6, BBox: &world})
	if !slices.Equal(sortedIDs(paged), sortedIDs(overview)) {
		t.Fatal("paging the overview changed its features")
	}
	expr, err := cql.Parse("id = 'paris-0001'")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Items(ctx, name, Query{Limit: 5, Level: 8}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("level 8 overview: %v", err)
	}
	if _, err := l.Items(ctx, name, Query{Limit: 5, Level: 6, Filter: expr}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overview with a filter: %v", err)
	}
}
