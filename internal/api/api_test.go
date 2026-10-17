package api

import (
	"net/http/httptest"
	"testing"

	"github.com/adonm/postfgb/internal/tiles"
)

func TestTilesForBounds(t *testing.T) {
	x0, x1, y0, y1 := tiles.BoundsFor([4]float64{-180, -85, 180, 85}, 0)
	if x0 != 0 || x1 != 0 || y0 != 0 || y1 != 0 {
		t.Fatalf("z0 = %d %d %d %d", x0, x1, y0, y1)
	}
	x0, x1, y0, y1 = tiles.BoundsFor([4]float64{-180, -85, 180, 85}, 2)
	if x0 != 0 || x1 != 3 || y0 != 0 || y1 != 3 {
		t.Fatalf("z2 = %d %d %d %d", x0, x1, y0, y1)
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
		{"GET", "/collections/demo/chunks/1"},
		{"GET", "/collections/demo/manifest"},
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

func TestParseFilters(t *testing.T) {
	r := httptest.NewRequest("GET", "/collections/demo/items?bbox=1,2,3,4&q=road&kind=highway&limit=5&filter=i%3E1", nil)
	filters := parseFilters(r)
	if len(filters) != 1 || filters["kind"] != "highway" {
		t.Fatalf("filters = %v", filters)
	}
}
