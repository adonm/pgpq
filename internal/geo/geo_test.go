package geo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/wkb"
)

func pointAt(t *testing.T, x, y float64) []byte {
	t.Helper()
	raw, err := wkb.Marshal(geom.NewPointFlat(geom.XY, []float64{x, y}), wkb.NDR)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDatasetRoundTrip(t *testing.T) {
	cols := []Column{
		{Name: "id", Type: String},
		{Name: "n", Type: Int},
		{Name: "score", Type: Double},
		{Name: "tags", Type: JSON},
	}
	features := []Feature{
		{ID: "a", Geometry: pointAt(t, 2.35, 48.85), Props: map[string]any{"id": "a", "n": int64(1), "score": 1.5, "tags": `{"k":1}`}, Key: 10},
		{ID: "b", Geometry: pointAt(t, 2.36, 48.86), Props: map[string]any{"id": "b", "n": int64(2), "score": 2.5, "tags": `{"k":2}`}, Key: 20},
	}
	for i := range features {
		features[i].Bounds, _ = Bounds(features[i].Geometry)
	}
	data, err := EncodeShard(Dataset{Columns: cols, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := ReadDataset(bytes.NewReader(data), Options{IDColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if ds.Geometry != "geometry" || ds.IDColumn != "id" || len(ds.Features) != 2 {
		t.Fatalf("dataset = %+v", ds)
	}
	if ds.Features[0].ID != "a" || !bytes.Equal(ds.Features[0].Geometry, features[0].Geometry) {
		t.Fatalf("first feature = %+v", ds.Features[0])
	}
	if ds.Features[1].Props["n"] != int64(2) || ds.Features[1].Props["score"] != 2.5 {
		t.Fatalf("second props = %+v", ds.Features[1].Props)
	}
	if string(ds.Features[1].Props["tags"].(json.RawMessage)) != `{"k":2}` {
		t.Fatalf("tags = %v", ds.Features[1].Props["tags"])
	}
}

func TestEncodeShardRowGroups(t *testing.T) {
	cols := []Column{{Name: "id", Type: String}}
	features := make([]Feature, rowGroupRows+1)
	for i := range features {
		features[i] = Feature{
			Geometry: pointAt(t, float64(i)/1000, 48),
			Props:    map[string]any{"id": fmt.Sprintf("f%05d", i)},
			Key:      uint64(i),
		}
		features[i].Bounds, _ = Bounds(features[i].Geometry)
	}
	data, err := EncodeShard(Dataset{Columns: cols, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(file.RowGroups()); got != 2 {
		t.Fatalf("row groups = %d, want 2", got)
	}
	if file.NumRows() != int64(len(features)) {
		t.Fatalf("rows = %d", file.NumRows())
	}
}

func TestPROJJSONCRS(t *testing.T) {
	crs := `{"$schema":"https://proj.org/schemas/v0.5/projjson.schema.json","type":"GeographicCRS","name":"WGS 84 (CRS84)","id":{"authority":"OGC","code":"CRS84"}}`
	if err := checkCRS(crs); err != nil {
		t.Fatal(err)
	}
	if err := checkCRS(`{"id":{"authority":"EPSG","code":3857}}`); err == nil {
		t.Fatal("Web Mercator accepted")
	}
}
