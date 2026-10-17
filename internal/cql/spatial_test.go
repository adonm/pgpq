package cql

import (
	"strings"
	"testing"
)

func TestParseSpatial(t *testing.T) {
	expr, err := Parse(`name = 'x' and S_INTERSECTS(geometry, POLYGON((0 0,10 0,10 10,0 10,0 0)))`)
	if err != nil {
		t.Fatal(err)
	}
	rest, spatials, err := ExtractSpatial(expr)
	if err != nil {
		t.Fatal(err)
	}
	if len(spatials) != 1 {
		t.Fatalf("spatials = %d", len(spatials))
	}
	var args []any
	sql := rest.SQL("props", &args)
	if strings.Contains(sql, "true") || sql == "" {
		t.Fatalf("unexpected residual SQL: %q", sql)
	}
	stringForm, err := Parse(`S_INTERSECTS(geom, 'MULTIPOLYGON(((0 0,1 0,1 1,0 0)))')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, spatials, err := ExtractSpatial(stringForm); err != nil || len(spatials) != 1 {
		t.Fatalf("string WKT form: %v %d", err, len(spatials))
	}
	orForm, err := Parse(`S_INTERSECTS(geometry, POLYGON((0 0,1 0,1 1,0 1,0 0))) or name = 'x'`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ExtractSpatial(orForm); err == nil {
		t.Fatal("spatial under OR should be rejected")
	}
	notForm, err := Parse(`not S_INTERSECTS(geometry, POLYGON((0 0,1 0,1 1,0 1,0 0)))`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ExtractSpatial(notForm); err == nil {
		t.Fatal("negated spatial should be rejected")
	}
	if _, err := Parse(`S_INTERSECTS(geometry, CIRCLE(0 0,1))`); err == nil {
		t.Fatal("unsupported geometry type should be rejected")
	}
}

func TestParseSpatialJSON(t *testing.T) {
	body := []byte(`{"op":"and","args":[{"op":"=","args":[{"property":"name"},"x"]},{"op":"s_intersects","args":[{"property":"geometry"},{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}]}]}`)
	expr, err := ParseJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	rest, spatials, err := ExtractSpatial(expr)
	if err != nil {
		t.Fatal(err)
	}
	if len(spatials) != 1 || rest == nil {
		t.Fatalf("spatials = %d, rest = %v", len(spatials), rest)
	}
	if _, err := ParseJSON([]byte(`{"op":"s_intersects","args":[{"property":"geometry"},{"type":"Point","coordinates":[1,2]}]}`)); err == nil {
		t.Fatal("Point geometry should be rejected")
	}
}
