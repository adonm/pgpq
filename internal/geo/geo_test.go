package geo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/wkb"

	"github.com/adonm/pgpq/internal/cql"
)

func pointAt(t *testing.T, x, y float64) []byte {
	t.Helper()
	raw, err := wkb.Marshal(geom.NewPointFlat(geom.XY, []float64{x, y}), wkb.NDR)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCellRoundTrip(t *testing.T) {
	point := func(x, y float64) []byte { return pointAt(t, x, y) }
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
	file, err := Open(bytes.NewReader(data), int64(len(data)))
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

func TestReadDatasetRoundTrip(t *testing.T) {
	cols := []Column{
		{Name: "id", Type: cql.String},
		{Name: "n", Type: cql.Int},
		{Name: "score", Type: cql.Double},
		{Name: "tags", Type: cql.JSON},
	}
	features := []Feature{
		{ID: "a", Geometry: pointAt(t, 2.35, 48.85), Props: map[string]any{"id": "a", "n": int64(1), "score": 1.5, "tags": `{"k":1}`}, Key: 10},
		{ID: "b", Geometry: pointAt(t, 2.36, 48.86), Props: map[string]any{"id": "b", "n": int64(2), "score": 2.5, "tags": `{"k":2}`}, Key: 20},
	}
	for i := range features {
		features[i].Bounds, _ = Bounds(features[i].Geometry)
	}
	data, err := EncodeCell(Dataset{Columns: cols, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := ReadDataset(bytes.NewReader(data), Options{IDColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if ds.Geometry != "geometry" || ds.IDColumn != "id" {
		t.Fatalf("dataset = %q %q", ds.Geometry, ds.IDColumn)
	}
	if len(ds.Columns) != len(cols) {
		t.Fatalf("columns = %+v", ds.Columns)
	}
	if len(ds.Features) != len(features) {
		t.Fatalf("features = %d", len(ds.Features))
	}
	for i, feature := range ds.Features {
		if feature.ID != features[i].ID {
			t.Fatalf("feature %d = %+v", i, feature)
		}
		if !bytes.Equal(feature.Geometry, features[i].Geometry) {
			t.Fatalf("feature %d geometry changed", i)
		}
		if feature.Props["n"] != int64(i+1) || feature.Props["score"] != float64(i)+1.5 {
			t.Fatalf("feature %d props = %+v", i, feature.Props)
		}
		if string(feature.Props["tags"].(json.RawMessage)) != fmt.Sprintf(`{"k":%d}`, i+1) {
			t.Fatalf("feature %d tags = %v", i, feature.Props["tags"])
		}
	}
}

func TestEncodeCellRowGroups(t *testing.T) {
	cols := []Column{{Name: "id", Type: cql.String}}
	features := make([]Feature, rowGroupRows+1)
	for i := range features {
		features[i] = Feature{
			Geometry: pointAt(t, float64(i)/1000, 48),
			Props:    map[string]any{"id": fmt.Sprintf("f%05d", i)},
			Key:      uint64(i),
		}
		features[i].Bounds, _ = Bounds(features[i].Geometry)
	}
	data, err := EncodeCell(Dataset{Columns: cols, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	file, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got := file.GroupCount(); got != 2 {
		t.Fatalf("row groups = %d, want 2", got)
	}
	got, err := file.RowGroups([]int{0, 1}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(features) || got[len(got)-1].ID != "f02048" {
		t.Fatalf("rows = %d, last = %q", len(got), got[len(got)-1].ID)
	}
}

func TestCountAttributes(t *testing.T) {
	cols := []Column{{Name: "id", Type: cql.String}, {Name: "n", Type: cql.Int}}
	features := []Feature{
		{Geometry: pointAt(t, 1, 1), Props: map[string]any{"id": "a", "n": int64(1)}, Key: 1},
		{Geometry: pointAt(t, 2, 2), Props: map[string]any{"id": "b"}, Key: 2},
		{Geometry: pointAt(t, 3, 3), Props: map[string]any{"id": "c", "n": int64(3)}, Key: 3},
	}
	for i := range features {
		features[i].Bounds, _ = Bounds(features[i].Geometry)
	}
	data, err := EncodeCell(Dataset{Columns: cols, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	file, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(features))
	matched, err := file.Count([]int{0}, func(props map[string]any) bool {
		id := props["id"].(string)
		_, hasN := props["n"]
		if hasN != (id != "b") {
			t.Fatalf("row %q has n = %v", id, hasN)
		}
		seen[id] = true
		return hasN
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(features) || matched != 2 {
		t.Fatalf("seen %d features, matched %d", len(seen), matched)
	}
}
