package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
)

func TestDecideJSONColumns(t *testing.T) {
	cols := []chunk.Column{
		{Name: "name", Type: flattypes.ColumnTypeString},
		{Name: "names", Type: flattypes.ColumnTypeString},
		{Name: "plain", Type: flattypes.ColumnTypeString},
		{Name: "sparse", Type: flattypes.ColumnTypeString},
	}
	features := make([]chunk.InputFeature, 0, 16)
	for i := 0; i < 16; i++ {
		props := map[string]json.RawMessage{
			"name":  json.RawMessage(`"[ \"Ship and Castle\" ]"`),
			"names": json.RawMessage(`"{\"en\":\"Ship\"}"`),
			"plain": json.RawMessage(`"Ship and Castle"`),
		}
		if i == 0 {
			props["sparse"] = json.RawMessage(`"[1,2]"`)
		}
		features = append(features, chunk.InputFeature{Properties: props})
	}
	decideJSONColumns(cols, features)
	want := map[string]flattypes.ColumnType{
		"name":   flattypes.ColumnTypeJson,
		"names":  flattypes.ColumnTypeJson,
		"plain":  flattypes.ColumnTypeString,
		"sparse": flattypes.ColumnTypeString,
	}
	for _, col := range cols {
		if col.Type != want[col.Name] {
			t.Fatalf("%s type = %v, want %v", col.Name, col.Type, want[col.Name])
		}
	}
	rewriteJSONValues(cols, features)
	if string(features[0].Properties["name"]) != `[ "Ship and Castle" ]` {
		t.Fatalf("name = %s", features[0].Properties["name"])
	}
	if string(features[0].Properties["names"]) != `{"en":"Ship"}` {
		t.Fatalf("names = %s", features[0].Properties["names"])
	}
	if string(features[0].Properties["plain"]) != `"Ship and Castle"` {
		t.Fatalf("plain = %s", features[0].Properties["plain"])
	}
}

func TestJSONText(t *testing.T) {
	cases := []struct {
		in    string
		value string
		ok    bool
	}{
		{`"[1,2]"`, `[1,2]`, true},
		{`"  {\"a\": 1}  "`, `{"a": 1}`, true},
		{`"plain"`, "", false},
		{`"[]"`, `[]`, true},
		{`"[unterminated"`, "", false},
		{`42`, "", false},
		{`null`, "", false},
	}
	for _, tc := range cases {
		value, ok := jsonText(json.RawMessage(tc.in))
		if ok != tc.ok {
			t.Fatalf("jsonText(%s) ok = %v, want %v", tc.in, ok, tc.ok)
		}
		if ok && string(value) != tc.value {
			t.Fatalf("jsonText(%s) = %s, want %s", tc.in, value, tc.value)
		}
	}
}

func TestFlattenJSONColumns(t *testing.T) {
	cols := []chunk.Column{
		{Name: "names", Type: flattypes.ColumnTypeJson},
		{Name: "id", Type: flattypes.ColumnTypeLong},
	}
	features := []chunk.InputFeature{
		{Properties: map[string]json.RawMessage{"names": json.RawMessage(`{"en":"Paris","fr":"Paris","pop":2148000}`)}},
		{Properties: map[string]json.RawMessage{"names": json.RawMessage(`{"en":"Lyon","pop":515695,"capital":false}`)}},
		{Properties: map[string]json.RawMessage{"names": json.RawMessage(`{"en":"Nice"}`)}},
	}
	out, flat := flattenJSONColumns(cols, features)
	flat.apply(features)
	want := map[string]flattypes.ColumnType{"names_en": flattypes.ColumnTypeString, "names_pop": flattypes.ColumnTypeLong}
	if len(out) != len(cols)+len(want) {
		t.Fatalf("columns = %d, want %d", len(out), len(cols)+len(want))
	}
	for _, col := range out[len(cols):] {
		typ, ok := want[col.Name]
		if !ok {
			t.Fatalf("unexpected flattened column %q", col.Name)
		}
		if col.Type != typ {
			t.Fatalf("%s type = %v, want %v", col.Name, col.Type, typ)
		}
	}
	if got := string(features[0].Properties["names_en"]); got != `"Paris"` {
		t.Fatalf("names_en = %s", got)
	}
	if got := string(features[1].Properties["names_pop"]); got != "515695" {
		t.Fatalf("names_pop = %s", got)
	}
	if _, ok := features[2].Properties["names_pop"]; ok {
		t.Fatal("missing key should stay absent")
	}
	if _, ok := features[0].Properties["names_fr"]; ok {
		t.Fatal("a key below the support threshold should not be flattened")
	}
	later := []chunk.InputFeature{{Properties: map[string]json.RawMessage{"names": json.RawMessage(`{"en":"Metz","pop":119000}`)}}}
	flat.apply(later)
	if got := string(later[0].Properties["names_pop"]); got != "119000" {
		t.Fatalf("a later window names_pop = %s, want 119000", got)
	}
}

func TestChunkByteCapSplits(t *testing.T) {
	cols := []chunk.Column{{Name: "name", Type: flattypes.ColumnTypeString}}
	batch := make([]chunk.InputFeature, 0, 200)
	for i := 0; i < 200; i++ {
		coords, err := json.Marshal([]float64{float64(i), 0})
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, chunk.InputFeature{
			Geometry:   chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{"name": json.RawMessage(fmt.Sprintf("%q", strings.Repeat("x", 200)))},
		})
	}
	var sizes []int
	if err := encodeChunks("cap", cols, batch, 4096, nil, func(data []byte) error {
		sizes = append(sizes, len(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sizes) < 2 {
		t.Fatalf("cap produced %d chunks, want the stream split", len(sizes))
	}
	for _, size := range sizes {
		if size > 4096 {
			t.Fatalf("chunk of %d bytes exceeds the %d byte cap", size, 4096)
		}
	}
}
