package api

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
)

func TestPostgresAttrSidecar(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_attr"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeLong},
		{Name: "label", Type: flattypes.ColumnTypeString},
		{Name: "flag", Type: flattypes.ColumnTypeBool},
		{Name: "tags", Type: flattypes.ColumnTypeJson},
	}
	features := make([]chunk.InputFeature, 0, 500)
	for i := 0; i < 500; i++ {
		coords, err := json.Marshal([]float64{float64(i%20) * 0.1, float64(i/20) * 0.1})
		if err != nil {
			t.Fatal(err)
		}
		features = append(features, chunk.InputFeature{
			Geometry: chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"id":    json.RawMessage(strconv.Itoa(i)),
				"label": json.RawMessage(strconv.Quote("n-" + strconv.Itoa(i%7))),
				"flag":  json.RawMessage(strconv.FormatBool(i%2 == 0)),
				"tags":  json.RawMessage(`["a","b"]`),
			},
		})
	}
	if w := call(New(st), "PUT", "/collections/"+name, "application/flatgeobuf", mustEncode(t, name, cols, features)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	var data []byte
	if err := st.Pool.QueryRow(ctx, `select data from attr_chunks where collection = $1`, name).Scan(&data); err != nil {
		t.Fatal(err)
	}
	file, ok := chunk.ReadAttrHeader(data)
	if !ok || file.Rows != 500 {
		t.Fatalf("sidecar header rows=%d ok=%v", file.Rows, ok)
	}
	labels, err := chunk.DecodeAttrColumn(data, "label")
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[string]bool{}
	for _, value := range labels {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("label %T", value)
		}
		distinct[text] = true
	}
	if len(distinct) != 7 {
		t.Fatalf("label distinct %d", len(distinct))
	}
	flags, err := chunk.DecodeAttrColumn(data, "flag")
	if err != nil {
		t.Fatal(err)
	}
	trueCount := 0
	for _, value := range flags {
		if value == true {
			trueCount++
		}
	}
	if trueCount != 250 {
		t.Fatalf("flag true count %d", trueCount)
	}
	tags, err := chunk.DecodeAttrColumn(data, "tags")
	if err != nil {
		t.Fatal(err)
	}
	members, ok := tags[0].([]any)
	if !ok || len(members) != 2 || members[0] != "a" || members[1] != "b" {
		t.Fatalf("tags decode %v", tags[0])
	}
}

func mustEncode(t *testing.T, name string, cols []chunk.Column, features []chunk.InputFeature) []byte {
	t.Helper()
	body, err := chunk.EncodeCols(name, cols, features)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
