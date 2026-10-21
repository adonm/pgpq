package geo

import (
	"bytes"
	"testing"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/wkb"

	"github.com/adonm/pgpq/internal/cql"
)

func TestCellRoundTrip(t *testing.T) {
	point := func(x, y float64) []byte {
		raw, err := wkb.Marshal(geom.NewPointFlat(geom.XY, []float64{x, y}), wkb.NDR)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cols := []Column{{Name: "id", Type: cql.String}, {Name: "n", Type: cql.Int}}
	features := []Feature{
		{Geometry: point(2.35, 48.85), Props: map[string]any{"id": "a", "n": int64(1)}, Key: 10},
		{Geometry: point(2.36, 48.86), Props: map[string]any{"id": "b", "n": int64(2)}, Key: 20},
	}
	for i := range features {
		features[i].Bounds, _ = Bounds(features[i].Geometry)
	}
	data, err := EncodeCell(Dataset{Columns: cols, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("PAR1")) {
		t.Fatal("cell is not a Parquet file")
	}
	file, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	box, ok := file.GroupBBox(0)
	if !ok || box[0] != 2.35 || box[2] != 2.36 {
		t.Fatalf("group bbox = %v %v", box, ok)
	}
	got, err := file.RowGroups([]int{0}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].Props["n"] != int64(2) {
		t.Fatalf("rows = %+v", got)
	}
}
