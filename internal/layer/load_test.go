package layer

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"slices"
	"testing"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
)

func testFeature(id string, key uint64) geo.Feature {
	x := float64(key%180) - 90
	y := float64(key%80) - 40
	return geo.Feature{ID: id, Key: key, Bounds: [4]float64{x, y, x, y}}
}

func TestIDHashMatchesFNV(t *testing.T) {
	for _, id := range []string{"", "a", "monaco-123", "é", "a-very-long-feature-identifier"} {
		h := fnv.New64a()
		h.Write([]byte(id))
		if got := idHash(id); got != h.Sum64() {
			t.Fatalf("idHash(%q) = %d, want %d", id, got, h.Sum64())
		}
	}
}

func TestMergeFeaturesReplacesIncomingIDs(t *testing.T) {
	old := []geo.Feature{testFeature("a", 1), testFeature("b", 2), testFeature("c", 3)}
	incoming := []geo.Feature{testFeature("b", 4), testFeature("d", 5)}
	merged := mergeFeatures(old, incoming)
	if len(merged) != 4 {
		t.Fatalf("merged %d features, want 4", len(merged))
	}
	keys := make(map[string]uint64, len(merged))
	for _, feature := range merged {
		keys[feature.ID] = feature.Key
	}
	if keys["a"] != 1 || keys["b"] != 4 || keys["c"] != 3 || keys["d"] != 5 {
		t.Fatalf("merged keys = %v", keys)
	}
}

func referenceSample(features []geo.Feature) []geo.Feature {
	sampled := make([]geo.Feature, 0, len(features))
	for _, feature := range features {
		if !sampleTooSmall(feature.Bounds, 0) {
			sampled = append(sampled, feature)
		}
	}
	slices.SortFunc(sampled, func(a, b geo.Feature) int {
		if c := cmp.Compare(idHash(a.ID), idHash(b.ID)); c != 0 {
			return c
		}
		return compareKeyID(a.Key, a.ID, b.Key, b.ID)
	})
	sampled = sampled[:min(len(sampled), overviewSampleCap)]
	slices.SortFunc(sampled, func(a, b geo.Feature) int { return compareKeyID(a.Key, a.ID, b.Key, b.ID) })
	return sampled
}

func TestSampleFeaturesMatchesFullSort(t *testing.T) {
	const n = 10000
	features := make([]geo.Feature, n)
	for i := range features {
		features[i] = testFeature(fmt.Sprintf("f%05d", i), uint64(i*7919))
	}
	want := referenceSample(features)
	input := slices.Clone(features)
	got := sampleFeatures(input, 0)
	if !slices.EqualFunc(want, got, func(a, b geo.Feature) bool { return a.ID == b.ID }) {
		t.Fatalf("sample differs from the full sort")
	}
	split := 6000
	incremental := sampleFeatures(features[:split], 0)
	incremental = append(incremental, sampleFeatures(features[split:], 0)...)
	incremental = sampleFeatures(incremental, 0)
	if !slices.EqualFunc(want, incremental, func(a, b geo.Feature) bool { return a.ID == b.ID }) {
		t.Fatal("per-cell sampling differs from sampling the union")
	}
	if !slices.EqualFunc(input, features, func(a, b geo.Feature) bool { return a.ID == b.ID && a.Key == b.Key }) {
		t.Fatal("sampleFeatures mutated its input")
	}
}

func TestGroupFeaturesByCell(t *testing.T) {
	const n = 500
	features := make([]geo.Feature, n)
	for i := range features {
		features[i] = testFeature(fmt.Sprintf("f%04d", i), uint64(i))
	}
	entries, err := indexFeatures(features, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := groupFeatures(features, entries)
	total := 0
	for cell, group := range groups {
		if spatial.Partition(group[0].Key) != cell {
			t.Fatalf("group %d holds key %d", cell, group[0].Key)
		}
		total += len(group)
	}
	if total != n || len(groups) == 0 {
		t.Fatalf("grouped %d of %d features into %d cells", total, n, len(groups))
	}
}

func TestIndexFeaturesRejectsDuplicates(t *testing.T) {
	features := []geo.Feature{testFeature("a", 1), testFeature("a", 2)}
	if _, err := indexFeatures(features, nil); err == nil {
		t.Fatal("duplicate ids were accepted")
	}
}

func TestIndexFeaturesHonoursListedCells(t *testing.T) {
	features := []geo.Feature{testFeature("a", 1), testFeature("b", 2)}
	cell := spatial.Partition(spatial.Key(features[0].Bounds))
	if _, err := indexFeatures(features, []uint64{cell}); err == nil {
		t.Fatal("a feature outside the listed cell was accepted")
	}
}
