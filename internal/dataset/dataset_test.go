package dataset

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/wkb"

	"github.com/adonm/pgpq/internal/geo"
)

func testFeatures(t *testing.T, count int) (geo.Dataset, []geo.Feature) {
	t.Helper()
	features := make([]geo.Feature, count)
	for i := range features {
		point, err := wkb.Marshal(geom.NewPointFlat(geom.XY, []float64{2.0 + float64(i)/1000, 48.0}), wkb.NDR)
		if err != nil {
			t.Fatal(err)
		}
		features[i] = geo.Feature{
			ID:       fmt.Sprintf("id-%03d", i),
			Geometry: point,
			Props:    map[string]any{"id": fmt.Sprintf("id-%03d", i), "n": int64(i)},
		}
		features[i].Bounds, _ = geo.Bounds(point)
	}
	return geo.Dataset{
		Columns:  []geo.Column{{Name: "id", Type: geo.String}, {Name: "n", Type: geo.Int}},
		Geometry: "geometry",
		IDColumn: "id",
		Features: features,
	}, features
}

func TestPrepareSortsAndRejectsDuplicateIDs(t *testing.T) {
	ds, _ := testFeatures(t, 3)
	got, err := prepare(ds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d features", len(got))
	}
	ds.Features[1].ID = ds.Features[0].ID
	ds.Features[1].Props["id"] = ds.Features[0].ID
	if _, err := prepare(ds); err == nil {
		t.Fatal("duplicate id accepted")
	}
}

func TestEncodeShardsSplits(t *testing.T) {
	ds, features := testFeatures(t, 20)
	sorted, err := prepare(ds)
	if err != nil {
		t.Fatal(err)
	}
	shards, err := encodeShards(ds, sorted, 1600)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) < 2 {
		t.Fatalf("encoded %d shards", len(shards))
	}
	rows := 0
	for i, shard := range shards {
		if len(shard.features) == 0 {
			t.Fatalf("shard %d is empty", i)
		}
		rows += len(shard.features)
		opened, err := geo.ReadDataset(bytes.NewReader(shard.data), geo.Options{IDColumn: "id"})
		if err != nil {
			t.Fatal(err)
		}
		if len(opened.Features) != len(shard.features) {
			t.Fatalf("shard %d has %d rows, want %d", i, len(opened.Features), len(shard.features))
		}
	}
	if rows != len(features) {
		t.Fatalf("shards hold %d rows, want %d", rows, len(features))
	}
}
