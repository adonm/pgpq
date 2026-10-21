package layer

import (
	"testing"

	"github.com/adonm/pgpq/internal/cql"
	"github.com/adonm/pgpq/internal/geo"
)

func TestZoneRoundTrip(t *testing.T) {
	cols := []geo.Column{
		{Name: "id", Type: cql.String},
		{Name: "n", Type: cql.Int},
		{Name: "score", Type: cql.Double},
	}
	features := []geo.Feature{
		{Props: map[string]any{"id": "a", "n": int64(5), "score": 2.5}},
		{Props: map[string]any{"id": "b", "score": -1.5}},
		{Props: map[string]any{"id": "c", "n": int64(1), "score": 4.0}},
	}
	text, err := encodeZone(cols, features)
	if err != nil {
		t.Fatal(err)
	}
	zone, err := decodeZone(text)
	if err != nil {
		t.Fatal(err)
	}
	if got := zone["id"].Nulls; got != 0 {
		t.Fatalf("id nulls = %d", got)
	}
	if got := zone["n"].Nulls; got != 1 {
		t.Fatalf("n nulls = %d", got)
	}
	if zone["n"].Min == nil || *zone["n"].Min != 1 || zone["n"].Max == nil || *zone["n"].Max != 5 {
		t.Fatalf("n zone = %+v", zone["n"])
	}
	if zone["score"].Min == nil || *zone["score"].Min != -1.5 || zone["score"].Max == nil || *zone["score"].Max != 4 {
		t.Fatalf("score zone = %+v", zone["score"])
	}
	if zone["id"].Min != nil || zone["id"].Max != nil {
		t.Fatalf("string zone has bounds: %+v", zone["id"])
	}
}

func TestZoneSkipsNonNumericValues(t *testing.T) {
	cols := []geo.Column{{Name: "n", Type: cql.Int}}
	text, err := encodeZone(cols, []geo.Feature{{Props: map[string]any{"n": "not-a-number"}}})
	if err != nil {
		t.Fatal(err)
	}
	zone, err := decodeZone(text)
	if err != nil {
		t.Fatal(err)
	}
	if zone["n"].Nulls != 0 || zone["n"].Min != nil || zone["n"].Max != nil {
		t.Fatalf("zone = %+v", zone["n"])
	}
}

func TestMayMatchUsesZone(t *testing.T) {
	zone := map[string]zoneColumn{"n": {Name: "n", Min: ptr(2.0), Max: ptr(8.0)}}
	filter := &cql.Compare{Prop: "n", Op: ">", Value: int64(9)}
	if mayMatch(filter, zone) {
		t.Fatal("zone admitted an impossible comparison")
	}
	filter = &cql.Compare{Prop: "n", Op: "<", Value: int64(9)}
	if !mayMatch(filter, zone) {
		t.Fatal("zone rejected a possible comparison")
	}
}

func ptr(value float64) *float64 {
	return &value
}
