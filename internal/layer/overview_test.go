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
	for i := range overviewCap + 44 {
		id := strconv.Itoa(i)
		entries = append(entries, entry{key: key, id: id, hash: idHash(id), cell: spatial.Partition(key)})
	}
	sample := sampleOverview(entries)
	if len(sample) != overviewCap {
		t.Fatalf("sample holds %d, want %d", len(sample), overviewCap)
	}
	hashes := make([]uint64, len(entries))
	for i, e := range entries {
		hashes[i] = e.hash
	}
	slices.Sort(hashes)
	for _, e := range sample {
		if e.hash > hashes[overviewCap-1] {
			t.Fatalf("kept hash %x above the cut %x", e.hash, hashes[overviewCap-1])
		}
	}
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
	overview := readAll(t, l, name, Query{Limit: 64, Level: overviewLevel, BBox: &world})
	checkOrdered(t, overview)
	detailIDs := map[string]bool{}
	population := map[uint64]int{}
	for _, h := range detail {
		detailIDs[h.ID] = true
		population[spatial.Level(h.Key, overviewLevel)]++
	}
	kept := map[uint64]int{}
	for _, h := range overview {
		if !detailIDs[h.ID] {
			t.Fatalf("overview feature %s is not a detail feature", h.ID)
		}
		kept[spatial.Level(h.Key, overviewLevel)]++
	}
	for cell, n := range population {
		if want := min(n, overviewCap); kept[cell] != want {
			t.Fatalf("cell %x keeps %d features, want %d of %d", cell, kept[cell], want, n)
		}
	}
	if _, err := l.Lookup(ctx, name, detail[0].ID); err != nil {
		t.Fatalf("lookup of a detail feature with an overview present: %v", err)
	}
	paged := readAll(t, l, name, Query{Limit: 3, Level: overviewLevel, BBox: &world})
	if !slices.Equal(sortedIDs(paged), sortedIDs(overview)) {
		t.Fatal("paging the overview changed its features")
	}
	expr, err := cql.Parse("id = 'paris-0001'")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Items(ctx, name, Query{Limit: 5, Level: overviewLevel, Filter: expr}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overview with a filter: %v", err)
	}
}
