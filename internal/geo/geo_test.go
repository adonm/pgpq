package geo

import (
	"testing"

	"github.com/paulmach/orb"
)

func TestParseWKT(t *testing.T) {
	shape, err := ParseWKT("POLYGON((0 0,10 0,10 10,0 10,0 0))")
	if err != nil {
		t.Fatal(err)
	}
	if len(shape) != 1 || len(shape[0]) != 1 {
		t.Fatalf("shape = %+v", shape)
	}
	multi, err := ParseWKT("MULTIPOLYGON(((0 0,10 0,10 10,0 10,0 0)),((20 20,30 20,30 30,20 30,20 20)))")
	if err != nil {
		t.Fatal(err)
	}
	if len(multi) != 2 {
		t.Fatalf("multi = %+v", multi)
	}
	if _, err := ParseWKT("POINT(1 2)"); err == nil {
		t.Fatal("POINT should be rejected")
	}
	if _, err := ParseWKT("POLYGON((0 0,10 0,10 10,0 10,0 0)) trailing"); err == nil {
		t.Fatal("trailing input should be rejected")
	}
	if _, err := ParseWKT("POLYGON(0 0,10 0,10 10)"); err == nil {
		t.Fatal("missing parens should be rejected")
	}
}

func TestParseGeoJSON(t *testing.T) {
	shape, err := ParseGeoJSON([]byte(`{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]]]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !shape.Intersects(orb.Point{5, 5}) {
		t.Fatal("point should be inside")
	}
	multi, err := ParseGeoJSON([]byte(`{"type":"MultiPolygon","coordinates":[[[[0,0],[1,0],[1,1],[0,0]]],[[[5,5],[6,5],[6,6],[5,5]]]]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(multi) != 2 {
		t.Fatalf("multi = %+v", multi)
	}
	if _, err := ParseGeoJSON([]byte(`{"type":"Point","coordinates":[1,2]}`)); err == nil {
		t.Fatal("Point should be rejected")
	}
}

func TestPointPredicates(t *testing.T) {
	shape, err := ParseWKT("POLYGON((0 0,10 0,10 10,0 10,0 0),(4 4,6 4,6 6,4 6,4 4))")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		x, y float64
		want bool
	}{
		{1, 1, true},
		{5, 5, false},
		{4, 5, true},
		{0, 5, true},
		{10.1, 5, false},
		{-0.1, 5, false},
	}
	for _, c := range cases {
		if got := shape.Intersects(orb.Point{c.x, c.y}); got != c.want {
			t.Fatalf("point %v,%v = %v, want %v", c.x, c.y, got, c.want)
		}
	}
	if !shape.Intersects(orb.MultiPoint{{5, 5}, {1, 1}}) {
		t.Fatal("multipoint with one inside should intersect")
	}
	if shape.Intersects(orb.MultiPoint{{5, 5}, {20, 20}}) {
		t.Fatal("multipoint fully outside should not intersect")
	}
}

func TestLinePredicates(t *testing.T) {
	shape, err := ParseWKT("POLYGON((0 0,10 0,10 10,0 10,0 0),(4 4,6 4,6 6,4 6,4 4))")
	if err != nil {
		t.Fatal(err)
	}
	if !shape.Intersects(orb.LineString{{-5, 5}, {15, 5}}) {
		t.Fatal("crossing line should intersect")
	}
	if !shape.Intersects(orb.LineString{{1, 1}, {2, 2}}) {
		t.Fatal("line inside should intersect")
	}
	if shape.Intersects(orb.LineString{{20, 20}, {30, 30}}) {
		t.Fatal("outside line should not intersect")
	}
	if !shape.Intersects(orb.LineString{{5, 5}, {5, 12}}) {
		t.Fatal("line crossing the hole boundary should intersect")
	}
}

func TestPolygonPredicates(t *testing.T) {
	shape, err := ParseWKT("POLYGON((0 0,10 0,10 10,0 10,0 0))")
	if err != nil {
		t.Fatal(err)
	}
	square := func(x0, y0, x1, y1 float64) orb.Polygon {
		return orb.Polygon{orb.Ring{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}}
	}
	if !shape.Intersects(square(2, 2, 4, 4)) {
		t.Fatal("contained polygon should intersect")
	}
	if !shape.Intersects(square(-5, 5, 5, 6)) {
		t.Fatal("overlapping polygon should intersect")
	}
	if !shape.Intersects(square(-5, -5, 15, 15)) {
		t.Fatal("containing polygon should intersect")
	}
	if shape.Intersects(square(20, 20, 30, 30)) {
		t.Fatal("disjoint polygon should not intersect")
	}
	holed, err := ParseWKT("POLYGON((0 0,10 0,10 10,0 10,0 0),(4 4,6 4,6 6,4 6,4 4))")
	if err != nil {
		t.Fatal(err)
	}
	if holed.Intersects(square(4.2, 4.2, 5.8, 5.8)) {
		t.Fatal("polygon fully inside the hole should not intersect")
	}
	if !holed.Intersects(square(3, 3, 5, 5)) {
		t.Fatal("polygon overlapping the hole boundary should intersect")
	}
	if !holed.Intersects(square(1, 1, 2, 2)) {
		t.Fatal("polygon inside the shell should intersect")
	}
}
