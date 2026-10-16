package api

import (
	"net/http/httptest"
	"testing"
)

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

func TestParseFilters(t *testing.T) {
	r := httptest.NewRequest("GET", "/collections/demo/items?bbox=1,2,3,4&q=road&kind=highway&limit=5&filter=i%3E1", nil)
	filters := parseFilters(r)
	if len(filters) != 1 || filters["kind"] != "highway" {
		t.Fatalf("filters = %v", filters)
	}
}
