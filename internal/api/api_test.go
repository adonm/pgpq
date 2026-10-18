package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

type collectSink struct {
	hits  []hit
	limit int
}

func (c *collectSink) add(h hit) error {
	c.hits = append(c.hits, h)
	return nil
}

func (c *collectSink) remaining() int {
	return c.limit - len(c.hits)
}

func TestSkipChunks(t *testing.T) {
	metas := []store.ChunkMeta{{ID: 1, FeatureCount: 100}, {ID: 2, FeatureCount: 50}, {ID: 3, FeatureCount: 10}}
	cases := []struct {
		offset int
		ids    []int64
		starts []int
		rest   int
	}{
		{0, []int64{1, 2, 3}, []int{0, 0, 0}, 0},
		{100, []int64{2, 3}, []int{0, 0}, 0},
		{110, []int64{2, 3}, []int{10, 0}, 0},
		{120, []int64{2, 3}, []int{20, 0}, 0},
		{200, nil, nil, 40},
	}
	for _, tc := range cases {
		read, starts, rest := skipChunks(metas, tc.offset)
		var ids []int64
		for _, meta := range read {
			ids = append(ids, meta.ID)
		}
		if !slices.Equal(ids, tc.ids) || !slices.Equal(starts, tc.starts) || rest != tc.rest {
			t.Fatalf("offset %d: chunks %v starts %v rest %d, want %v %v %d", tc.offset, ids, starts, rest, tc.ids, tc.starts, tc.rest)
		}
	}
}

func TestEmitSkipsOnlyExactMatches(t *testing.T) {
	var features []chunk.InputFeature
	for i := 0; i < 10; i++ {
		coords, err := json.Marshal([]float64{float64(i), 0})
		if err != nil {
			t.Fatal(err)
		}
		features = append(features, chunk.InputFeature{Geometry: chunk.RawGeometry{Type: "Point", Coordinates: coords}})
	}
	body, err := chunk.Encode("unit", features)
	if err != nil {
		t.Fatal(err)
	}
	c, err := chunk.FromRaw(body)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	inside := &[4]float64{2.5, -1, 7.5, 1}
	cases := []struct {
		bbox   *[4]float64
		offset int
		limit  int
		want   []string
		rest   int
	}{
		{inside, 0, 10, []string{"9-3", "9-4", "9-5", "9-6", "9-7"}, 0},
		{inside, 2, 3, []string{"9-5", "9-6", "9-7"}, 0},
		{inside, 5, 3, nil, 0},
		{inside, 7, 3, nil, 2},
		{nil, 3, 2, []string{"9-3", "9-4"}, 0},
	}
	for _, tc := range cases {
		sink := &collectSink{limit: tc.limit}
		rest, err := emit(store.CollectionInfo{Name: "unit"}, tc.bbox, 9, c, all, ordinals(0, len(all)), tc.offset, sink)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range sink.hits {
			got = append(got, fmt.Sprint(h.geo.ID))
		}
		if !slices.Equal(got, tc.want) || rest != tc.rest {
			t.Fatalf("offset %d limit %d: got %v rest %d, want %v rest %d", tc.offset, tc.limit, got, rest, tc.want, tc.rest)
		}
	}
}

func TestSplitBatches(t *testing.T) {
	ids := []int64{1, 2, 3, 4, 5}
	sizes := map[int64]int{1: 4, 2: 4, 3: 4, 4: 4, 5: 4}
	if got := splitBatches(ids, sizes, 2, 100); len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 {
		t.Fatalf("count cap: %v", got)
	}
	if got := splitBatches(ids, sizes, 10, 8); len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("byte cap: %v", got)
	}
	if got := splitBatches(ids, sizes, 10, 4); len(got) != 5 {
		t.Fatalf("single oversize chunk: %v", got)
	}
	if got := splitBatches(nil, nil, 10, 10); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestBudgetMetas(t *testing.T) {
	metas := []store.ChunkMeta{{ID: 1, SizeBytes: 4}, {ID: 2, SizeBytes: 4}, {ID: 3, SizeBytes: 4}}
	trimmed, cut := budgetMetas(metas, 100)
	if cut || len(trimmed) != 3 {
		t.Fatalf("no cut: %v %v", trimmed, cut)
	}
	trimmed, cut = budgetMetas(metas, 8)
	if !cut || len(trimmed) != 2 {
		t.Fatalf("cut: %v %v", trimmed, cut)
	}
	trimmed, cut = budgetMetas(metas, 4)
	if !cut || len(trimmed) != 1 {
		t.Fatalf("single: %v %v", trimmed, cut)
	}
}

func TestTypedLiteral(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"42", int64(42)},
		{"1.5", 1.5},
		{"true", true},
		{"false", false},
		{"feature-42", "feature-42"},
	}
	for _, tc := range cases {
		if got := typedLiteral(tc.in); got != tc.want {
			t.Fatalf("typedLiteral(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPageLink(t *testing.T) {
	r := httptest.NewRequest("GET", "/collections/demo/items?limit=100&bbox=1,2,3,4", nil)
	want := "/collections/demo/items?bbox=1%2C2%2C3%2C4&limit=100&offset=100"
	if got := pageLink(r, 100, 0, 100); got != want {
		t.Fatalf("link = %s, want %s", got, want)
	}
	if got := pageLink(r, 100, 0, 42); got != "" {
		t.Fatalf("partial page link = %q, want empty", got)
	}
}

func TestReadOnlyWorker(t *testing.T) {
	h := NewWithConfig(Config{ReadOnly: true})
	cases := []struct {
		method string
		path   string
	}{
		{"PUT", "/collections/demo"},
		{"DELETE", "/collections/demo"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 404 && w.Code != 405 {
			t.Fatalf("%s %s = %d, want 404 or 405", tc.method, tc.path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Fatalf("GET / = %d, want 200", w.Code)
	}
}

func TestRemovedRoutes(t *testing.T) {
	for _, h := range []http.Handler{NewWithConfig(Config{}), NewWithConfig(Config{ReadOnly: true})} {
		for _, path := range []string{"/stats", "/collections/demo/manifest", "/collections/demo/chunks/1", "/stac", "/stac/search", "/stac/collections/demo/items"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 404 {
				t.Fatalf("GET %s = %d, want 404", path, w.Code)
			}
		}
	}
}

func TestUnsupportedQuery(t *testing.T) {
	gate := rejectUnsupportedQuery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cases := []struct {
		target string
		want   int
	}{
		{"/collections/demo/items", 200},
		{"/collections/demo/items?sortby=name", 400},
		{"/collections/demo/items?sortby=", 400},
		{"/collections/demo/items?q=road", 400},
		{"/collections/demo/items?q=", 400},
		{"/collections/demo/items?crs=" + crs84, 200},
		{"/collections/demo/items?crs=EPSG:3857", 400},
		{"/collections/demo/items?bbox=1,2,3,4&bbox-crs=" + crs84, 200},
		{"/collections/demo/items?bbox=1,2,3,4&bbox-crs=EPSG:3857", 400},
		{"/collections/demo/items?filter=i%3E1", 200},
		{"/collections/demo/items?filter=i%3E1&filter-lang=cql2-text", 200},
		{"/collections/demo/items?filter-lang=cql2-json", 400},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest("GET", tc.target, nil))
		if w.Code != tc.want {
			t.Fatalf("%s = %d, want %d", tc.target, w.Code, tc.want)
		}
	}
}

func TestGeometryClass(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Point", "Point"},
		{"MultiPoint", "Point"},
		{"LineString", "Polyline"},
		{"MultiLineString", "Polyline"},
		{"Polygon", "Polygon"},
		{"MultiPolygon", "Polygon"},
	}
	for _, tc := range cases {
		if got := geometryClass(tc.in); got != tc.want {
			t.Fatalf("geometryClass(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestLandingLinks(t *testing.T) {
	w := httptest.NewRecorder()
	NewWithConfig(Config{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "/stac") {
		t.Fatalf("landing = %d, links to /stac: %s", w.Code, w.Body.String())
	}
}

func TestParseFilters(t *testing.T) {
	r := httptest.NewRequest("GET", "/collections/demo/items?bbox=1,2,3,4&kind=highway&limit=5&filter=i%3E1", nil)
	filters := parseFilters(r)
	if len(filters) != 1 || filters["kind"] != "highway" {
		t.Fatalf("filters = %v", filters)
	}
}
