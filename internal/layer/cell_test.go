package layer

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/spatial"
)

func denseFeatures(prefix string, lng, lat, spread float64, n int, seed int64) []chunk.InputFeature {
	rng := rand.New(rand.NewSource(seed))
	out := make([]chunk.InputFeature, n)
	for i := range out {
		x := lng + (rng.Float64()-0.5)*spread
		y := lat + (rng.Float64()-0.5)*spread
		props := map[string]json.RawMessage{
			"id":   json.RawMessage(strconv.Quote(fmt.Sprintf("%s-%05d", prefix, i))),
			"name": json.RawMessage(strconv.Quote("dense")),
		}
		out[i] = testPoint(testColumns, props, x, y)
	}
	return out
}

func featureIDsIn(t *testing.T, features []chunk.InputFeature, bbox [4]float64) []string {
	t.Helper()
	var ids []string
	for i := range features {
		if intersects(features[i].Bounds, &bbox) {
			ids = append(ids, string(features[i].Properties["id"]))
		}
	}
	for i, id := range ids {
		unquoted, err := strconv.Unquote(id)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = unquoted
	}
	slices.Sort(ids)
	return ids
}

func TestPerCellFilesFenceAndReads(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_cell_files"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
		{Name: "n", Type: flattypes.ColumnTypeLong},
		{Name: "score", Type: flattypes.ColumnTypeDouble},
		{Name: "flag", Type: flattypes.ColumnTypeBool},
	}
	features := withValues(denseFeatures("dense", 2.35, 48.85, 0.004, 5000, 101), 0)
	if _, err := l.Load(ctx, name, datasetOf(t, name, cols, features), nil); err != nil {
		t.Fatal(err)
	}
	rows, err := l.pool.Query(ctx, `select count(*), min(seq), max(seq), count(distinct file_id), count(distinct fence_file), sum(features) from pfgb.chunks where collection = $1 and level = 0 group by cell`, name)
	if err != nil {
		t.Fatal(err)
	}
	var files, minSeq, maxSeq, fgbs, fences, featuresStored int
	groups := 0
	for rows.Next() {
		if err := rows.Scan(&files, &minSeq, &maxSeq, &fgbs, &fences, &featuresStored); err != nil {
			t.Fatal(err)
		}
		groups++
	}
	rows.Close()
	if groups != 1 {
		t.Fatalf("detail has %d cells, want 1", groups)
	}
	if files != 1 || minSeq != 0 || maxSeq != 0 || fgbs != 1 || fences != 1 {
		t.Fatalf("detail cell rows: files=%d seq=%d..%d fgbs=%d fences=%d", files, minSeq, maxSeq, fgbs, fences)
	}
	if featuresStored != len(features) {
		t.Fatalf("detail stores %d features, want %d", featuresStored, len(features))
	}
	for _, level := range []int{4, 6} {
		var cells, levelRows int
		if err := l.pool.QueryRow(ctx, `select count(*), count(distinct cell) from pfgb.chunks where collection = $1 and level = $2`, name, level).Scan(&levelRows, &cells); err != nil {
			t.Fatal(err)
		}
		if cells != 1 || levelRows != 1 {
			t.Fatalf("level %d has %d rows over %d cells", level, levelRows, cells)
		}
	}
	var fenceFile int64
	if err := l.pool.QueryRow(ctx, `select fence_file from pfgb.chunks where collection = $1 and level = 0`, name).Scan(&fenceFile); err != nil {
		t.Fatal(err)
	}
	fenceData, err := l.blocks.ReadAll(ctx, fenceFile)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fgb.DecodeFence(fenceData)
	if err != nil {
		t.Fatal(err)
	}
	if want := (len(features) + fgb.FenceEvery - 1) / fgb.FenceEvery; fence.Len() != want {
		t.Fatalf("fence has %d entries, want %d", fence.Len(), want)
	}

	all := readAll(t, l, name, Query{Limit: 97})
	if len(all) != len(features) {
		t.Fatalf("detail read %d, want %d", len(all), len(features))
	}
	checkOrdered(t, all)
	allIDs := idsOf(all)

	bbox := [4]float64{2.3495, 48.8495, 2.3505, 48.8505}
	wantBox := featureIDsIn(t, features, bbox)
	gotBox := sortedIDs(readAll(t, l, name, Query{Limit: 5000, BBox: &bbox}))
	if !slices.Equal(gotBox, wantBox) {
		t.Fatalf("bbox returned %d ids, want %d", len(gotBox), len(wantBox))
	}
	if len(wantBox) == 0 || len(wantBox) >= len(features)/2 {
		t.Fatalf("bbox selected %d of %d features", len(wantBox), len(features))
	}

	paged := readAll(t, l, name, Query{Limit: 37})
	if !slices.Equal(idsOf(paged), allIDs) {
		t.Fatal("cursor paging changed the detail walk")
	}

	var wantFiltered []string
	for i, f := range features {
		if i >= 1000 && i <= 1100 {
			wantFiltered = append(wantFiltered, fmt.Sprintf("dense-%05d", i))
		}
		if f.Properties["n"] == nil {
			t.Fatal("test feature has no n")
		}
	}
	expr, err := cql.Parse("n BETWEEN 1000 AND 1100")
	if err != nil {
		t.Fatal(err)
	}
	gotFiltered := sortedIDs(readAll(t, l, name, Query{Limit: 5000, Filter: expr}))
	if !slices.Equal(gotFiltered, wantFiltered) {
		t.Fatalf("filter returned %d ids, want %d", len(gotFiltered), len(wantFiltered))
	}
	boxFiltered := sortedIDs(readAll(t, l, name, Query{Limit: 5000, Filter: expr, BBox: &bbox}))
	var wantBoxFiltered []string
	for _, id := range wantBox {
		var i int
		if _, err := fmt.Sscanf(id, "dense-%05d", &i); err != nil {
			t.Fatal(err)
		}
		if i >= 1000 && i <= 1100 {
			wantBoxFiltered = append(wantBoxFiltered, id)
		}
	}
	if !slices.Equal(boxFiltered, wantBoxFiltered) {
		t.Fatalf("bbox+filter returned %v, want %v", boxFiltered, wantBoxFiltered)
	}
	count, err := l.Count(ctx, name, expr)
	if err != nil || count != int64(len(wantFiltered)) {
		t.Fatalf("count = %d (%v), want %d", count, err, len(wantFiltered))
	}

	for _, i := range []int{0, 255, 256, 511, 4999} {
		id := fmt.Sprintf("dense-%05d", i)
		hit, err := l.Lookup(ctx, name, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if hit.ID != id {
			t.Fatalf("lookup %s returned %s", id, hit.ID)
		}
		bounds, ok := chunk.Bounds(hit.Header, hit.Feature)
		if !ok {
			t.Fatal("lookup feature has no bounds")
		}
		if spatial.Partition(spatial.Key(bounds)) != spatial.Partition(hit.Key) {
			t.Fatalf("%s resolved to another cell", id)
		}
	}
	if _, err := l.Lookup(ctx, name, "dense-99999"); err != ErrFeatureNotFound {
		t.Fatalf("missing lookup: %v", err)
	}
}

func TestPerCellSameKeyCursor(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_same_key"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := denseFeatures("same", 2.35, 48.85, 0, 900, 103)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, features), nil); err != nil {
		t.Fatal(err)
	}
	all := readAll(t, l, name, Query{Limit: 64})
	if len(all) != len(features) {
		t.Fatalf("same-key cell read %d, want %d", len(all), len(features))
	}
	checkOrdered(t, all)
	paged := readAll(t, l, name, Query{Limit: 37})
	if !slices.Equal(idsOf(paged), idsOf(all)) {
		t.Fatal("same-key cursor paging skipped or repeated rows")
	}
}
