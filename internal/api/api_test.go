package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adonm/postfgb/internal/store"
)

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
		for _, path := range []string{"/stats", "/collections/demo/manifest", "/collections/demo/chunks/1"} {
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

func TestStacSearchRejectsJSONFilter(t *testing.T) {
	h := NewWithConfig(Config{})
	for _, body := range []string{
		`{"filter":{"op":"=","args":[{"property":"amenity"},"bench"]}}`,
		`{"filter-lang":"cql2-json","filter":"amenity = 'bench'"}`,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/stac/search", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("%s = %d, want 400", body, w.Code)
		}
	}
}

func TestParseFilters(t *testing.T) {
	r := httptest.NewRequest("GET", "/collections/demo/items?bbox=1,2,3,4&kind=highway&limit=5&filter=i%3E1", nil)
	filters := parseFilters(r)
	if len(filters) != 1 || filters["kind"] != "highway" {
		t.Fatalf("filters = %v", filters)
	}
}
