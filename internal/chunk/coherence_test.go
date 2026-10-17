package chunk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
)

func TestStoredRoundTrip(t *testing.T) {
	features := make([]InputFeature, 200)
	for i := range features {
		coords, err := json.Marshal([]float64{float64(i%20)*0.5 - 5, float64(i/20)*0.25 - 2})
		if err != nil {
			t.Fatal(err)
		}
		features[i] = InputFeature{
			Geometry:   RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{"i": json.RawMessage(strconv.Itoa(i))},
		}
	}
	data, err := Encode("t", features)
	if err != nil {
		t.Fatal(err)
	}
	if IsCompressed(data) {
		t.Fatal("fresh encode should be raw")
	}
	stored := Compress(data)
	if !IsCompressed(stored) {
		t.Fatal("compressed payload not detected")
	}
	if len(stored) >= len(data) {
		t.Fatalf("compression did not shrink: %d vs %d", len(stored), len(data))
	}
	raw, err := Decompress(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, data) {
		t.Fatal("decompressed payload differs from original")
	}
	c, err := Open(stored)
	if err != nil {
		t.Fatal(err)
	}
	if c.Count() != len(features) {
		t.Fatalf("count = %d, want %d", c.Count(), len(features))
	}
	all, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(features) {
		t.Fatalf("search all = %d, want %d", len(all), len(features))
	}
}

func TestEncodeSearchCoherence(t *testing.T) {
	positions := [][2]float64{{40, 40}, {30, 30}, {20, 20}, {10, 10}, {0, 0}, {-10, -10}, {-20, -20}, {-30, -30}, {-40, -40}}
	features := make([]InputFeature, len(positions))
	for i, p := range positions {
		coords, err := json.Marshal([]float64{p[0], p[1]})
		if err != nil {
			t.Fatal(err)
		}
		features[i] = InputFeature{
			Geometry:   RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{"i": json.RawMessage(strconv.Itoa(i))},
		}
	}
	data, err := Encode("t", features)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := c.Search(5, 5, 45, 45)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("bbox hits = %d, want 4", len(hits))
	}
	for _, f := range hits {
		g := c.GeoJSON(f)
		coords, ok := g.Geometry.Coordinates.([]float64)
		if !ok {
			t.Fatalf("coordinates type %T", g.Geometry.Coordinates)
		}
		if coords[0] < 5 || coords[1] < 5 {
			t.Fatalf("hit outside bbox: %v", coords)
		}
	}
	all, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(features) {
		t.Fatalf("all = %d, want %d", len(all), len(features))
	}
	for i, f := range all {
		g := c.GeoJSON(f)
		if got := fmt.Sprint(g.Properties["i"]); got != strconv.Itoa(i) {
			t.Fatalf("ord %d: property i = %v", i, got)
		}
	}
}
