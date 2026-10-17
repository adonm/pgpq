package ingest

import (
	"encoding/json"
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
