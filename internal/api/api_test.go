package api

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/protomaps/go-pmtiles/pmtiles"
)

func TestTilesForBounds(t *testing.T) {
	x0, x1, y0, y1 := tilesForBounds([4]float64{-180, -85, 180, 85}, 0)
	if x0 != 0 || x1 != 0 || y0 != 0 || y1 != 0 {
		t.Fatalf("z0 = %d %d %d %d", x0, x1, y0, y1)
	}
	x0, x1, y0, y1 = tilesForBounds([4]float64{-180, -85, 180, 85}, 2)
	if x0 != 0 || x1 != 3 || y0 != 0 || y1 != 3 {
		t.Fatalf("z2 = %d %d %d %d", x0, x1, y0, y1)
	}
}

func TestBuildPMTiles(t *testing.T) {
	tiles := []tilePayload{
		{id: pmtiles.ZxyToID(0, 0, 0), data: []byte("tile0")},
		{id: pmtiles.ZxyToID(1, 1, 0), data: []byte("tile1")},
	}
	body, err := buildPMTiles("demo", 1, [4]float64{-180, -85, 180, 85}, nil, tiles)
	if err != nil {
		t.Fatal(err)
	}
	header, err := pmtiles.DeserializeHeader(body[:127])
	if err != nil {
		t.Fatal(err)
	}
	if header.SpecVersion != 3 || header.TileType != pmtiles.Mvt || header.AddressedTilesCount != 2 {
		t.Fatalf("header = %+v", header)
	}
	entries := pmtiles.DeserializeEntries(bytes.NewBuffer(body[header.RootOffset:header.RootOffset+header.RootLength]), header.InternalCompression)
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
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

func TestAdminOK(t *testing.T) {
	s := &Server{AdminToken: "secret"}
	if s.adminOK("") {
		t.Fatal("expected unauthorized without token")
	}
	if s.adminOK("Bearer wrong") {
		t.Fatal("expected unauthorized with wrong token")
	}
	if !s.adminOK("Bearer secret") {
		t.Fatal("expected authorized")
	}
	open := &Server{}
	if !open.adminOK("") {
		t.Fatal("expected open server to allow")
	}
}

func TestConvertExport(t *testing.T) {
	s := &Server{}
	if ok := s.adminOK(""); !ok {
		t.Fatal("expected open server")
	}
	if _, mime, err := s.convertExport(context.Background(), []byte("data"), "fgb"); err != nil || mime != "application/flatgeobuf" {
		t.Fatalf("passthrough: %v %q", err, mime)
	}
	if _, _, err := s.convertExport(context.Background(), []byte("data"), "nope"); err == nil {
		t.Fatal("expected unsupported format error")
	}
	if _, _, err := s.convertExport(context.Background(), []byte("data"), "gpkg"); err == nil {
		t.Fatal("expected no-converter error")
	}
}

func TestParseFilters(t *testing.T) {
	r := httptest.NewRequest("GET", "/collections/demo/items?bbox=1,2,3,4&q=road&kind=highway&limit=5&filter=i%3E1", nil)
	filters := parseFilters(r)
	if len(filters) != 1 || filters["kind"] != "highway" {
		t.Fatalf("filters = %v", filters)
	}
}
