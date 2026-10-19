package layer

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/spatial"
)

func TestLookupFindsAFeatureMovedWithinItsPartition(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_lookup_move"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	base := cityFeatures("paris", paris, 50, 41)
	mover := cityFeatures("mover", paris, 1, 42)[0]
	first := append(slices.Clone(base), mover)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, first), nil); err != nil {
		t.Fatal(err)
	}
	before, err := l.Lookup(ctx, name, "mover-0000")
	if err != nil {
		t.Fatal(err)
	}
	bounds, ok := chunk.FeatureBounds(&mover)
	if !ok {
		t.Fatal("feature without bounds")
	}
	coords, err := json.Marshal([]float64{bounds[0] + 0.0005, bounds[1] + 0.0005})
	if err != nil {
		t.Fatal(err)
	}
	moved := mover
	moved.Geometry.Coordinates = coords
	second := append(slices.Clone(base), moved)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, second), cellsOf(t, second)); err != nil {
		t.Fatal(err)
	}
	after, err := l.Lookup(ctx, name, "mover-0000")
	if err != nil {
		t.Fatalf("moved feature: %v", err)
	}
	if after.Key == before.Key || spatial.Partition(after.Key) != spatial.Partition(before.Key) {
		t.Fatalf("the test point left its partition: %x then %x", before.Key, after.Key)
	}
}
