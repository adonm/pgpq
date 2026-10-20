package layer

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
)

var filterTexts = []string{
	"n < 100",
	"n <= 100",
	"n > 100",
	"n >= 100",
	"n = 100",
	"n = 100.0",
	"n <> 5",
	"n BETWEEN 50 AND 450",
	"n NOT BETWEEN 50 AND 450",
	"n IN (1, 2, 250, 9999)",
	"score IS NULL",
	"score IS NOT NULL AND flag = true",
	"score > 0.5 AND flag = false",
	"score < 0.2 OR score > 0.8",
	"flag = false OR n < 3",
	"NOT (n > 10)",
	"name = 'dhaka' AND n >= 490",
	"name LIKE 'la%'",
	"id = 'paris-0007'",
	"missing IS NULL",
	"n > 5000",
}

func withValues(features []chunk.InputFeature, offset int) []chunk.InputFeature {
	for i := range features {
		props := features[i].Properties
		props["n"] = json.RawMessage(strconv.Itoa(offset + i))
		if i%7 != 0 {
			props["score"] = json.RawMessage(strconv.FormatFloat(float64(i%10)/10, 'f', -1, 64))
		}
		props["flag"] = json.RawMessage(strconv.FormatBool(i%3 == 0))
		x := (features[i].Bounds[0] + features[i].Bounds[2]) / 2
		y := (features[i].Bounds[1] + features[i].Bounds[3]) / 2
		features[i] = testPoint(denseCols, props, x, y)
	}
	return features
}

func matchingIDs(hits []Hit, expr cql.Expr) []string {
	var ids []string
	for _, hit := range hits {
		cols := chunk.HeaderColumns(hit.Header)
		props := map[string]any{}
		for name, raw := range chunk.DecodePropertyText(hit.Feature.PropertiesBytes(), cols) {
			var value any
			if json.Unmarshal(raw, &value) == nil {
				props[name] = value
			}
		}
		types := make(map[string]flattypes.ColumnType, len(cols))
		for _, col := range cols {
			types[col.Name] = col.Type
		}
		if cql.Match(expr, props, types) {
			ids = append(ids, hit.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func sortedIDs(hits []Hit) []string {
	ids := idsOf(hits)
	slices.Sort(ids)
	return ids
}

func TestItemsAndCountFollowFeatureValues(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_filter"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
		{Name: "n", Type: flattypes.ColumnTypeLong},
		{Name: "score", Type: flattypes.ColumnTypeDouble},
		{Name: "flag", Type: flattypes.ColumnTypeBool},
	}
	features := slices.Concat(
		withValues(cityFeatures("paris", paris, 300, 21), 0),
		withValues(cityFeatures("dhaka", dhaka, 300, 22), 200),
		withValues(cityFeatures("lagos", lagos, 150, 23), 400),
	)
	if _, err := l.Load(ctx, name, datasetOf(t, name, cols, features), nil); err != nil {
		t.Fatal(err)
	}
	all := readAll(t, l, name, Query{Limit: 64})
	if len(all) != len(features) {
		t.Fatalf("read %d features, loaded %d", len(all), len(features))
	}
	inBox := readAll(t, l, name, Query{Limit: 64, BBox: box(paris)})
	for _, text := range filterTexts {
		expr, err := cql.Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		want := matchingIDs(all, expr)
		got := readAll(t, l, name, Query{Limit: 64, Filter: expr})
		checkOrdered(t, got)
		if !slices.Equal(sortedIDs(got), want) {
			t.Fatalf("%s: got %d features, want %d", text, len(got), len(want))
		}
		count, err := l.Count(ctx, name, expr)
		if err != nil || count != int64(len(want)) {
			t.Fatalf("%s: count %d (%v), want %d", text, count, err, len(want))
		}
		wantBox := matchingIDs(inBox, expr)
		gotBox := readAll(t, l, name, Query{Limit: 64, BBox: box(paris), Filter: expr})
		if !slices.Equal(sortedIDs(gotBox), wantBox) {
			t.Fatalf("%s with a bbox: got %d features, want %d", text, len(gotBox), len(wantBox))
		}
	}
}

func TestZonePruningKeepsBoundaryValues(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_boundary"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "big", Type: flattypes.ColumnTypeLong},
	}
	features := cityFeatures("edge", paris, 3, 31)
	for i, big := range []string{"9007199254740992", "9007199254740993", "9007199254740994"} {
		features[i].Properties["big"] = json.RawMessage(big)
	}
	if _, err := l.Load(ctx, name, datasetOf(t, name, cols, features), nil); err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string][]string{
		"big < 9007199254740993":  {"edge-0000"},
		"big <= 9007199254740993": {"edge-0000", "edge-0001"},
		"big > 9007199254740993":  {"edge-0002"},
		"big >= 9007199254740993": {"edge-0001", "edge-0002"},
		"big = 9007199254740993":  {"edge-0001"},
	} {
		expr, err := cql.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		got := readAll(t, l, name, Query{Limit: 10, Filter: expr})
		if !slices.Equal(sortedIDs(got), want) {
			t.Fatalf("%s: got %v, want %v", text, sortedIDs(got), want)
		}
	}
}
