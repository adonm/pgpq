package api

import (
	"net/http/httptest"
	"testing"
)

func TestFilterDocument(t *testing.T) {
	doc, err := filterDocument(map[string]string{"name": "Main Street", "i": "42", "flag": "true"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"flag":true,"i":42,"name":"Main Street"}`
	if doc != want {
		t.Fatalf("doc = %s, want %s", doc, want)
	}
	empty, err := filterDocument(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Fatalf("empty doc = %q, want empty string", empty)
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
	r := httptest.NewRequest("GET", "/collections/demo/items?bbox=1,2,3,4&q=road&kind=highway&limit=5", nil)
	filters := parseFilters(r)
	if len(filters) != 1 || filters["kind"] != "highway" {
		t.Fatalf("filters = %v", filters)
	}
}
