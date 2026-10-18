package chunk

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

var corpusColumns = []Column{
	{"id", flattypes.ColumnTypeInt},
	{"c_bool", flattypes.ColumnTypeBool},
	{"c_byte", flattypes.ColumnTypeByte},
	{"c_ubyte", flattypes.ColumnTypeUByte},
	{"c_short", flattypes.ColumnTypeShort},
	{"c_ushort", flattypes.ColumnTypeUShort},
	{"c_uint", flattypes.ColumnTypeUInt},
	{"c_long", flattypes.ColumnTypeLong},
	{"c_ulong", flattypes.ColumnTypeULong},
	{"c_float", flattypes.ColumnTypeFloat},
	{"c_double", flattypes.ColumnTypeDouble},
	{"c_string", flattypes.ColumnTypeString},
	{"c_date", flattypes.ColumnTypeDateTime},
	{"c_json", flattypes.ColumnTypeString},
	{"c_tags", flattypes.ColumnTypeString},
}

func TestCorpusMixed(t *testing.T) {
	data, err := os.ReadFile("../../testdata/corpus/mixed.fgb")
	if err != nil {
		t.Fatal(err)
	}
	c, err := FromRaw(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Columns(); !reflect.DeepEqual(got, corpusColumns) {
		t.Fatalf("columns %v", got)
	}
	if c.Count() != 30000 {
		t.Fatalf("count %d", c.Count())
	}
	features, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 30000 {
		t.Fatalf("features %d", len(features))
	}
	seen := make(map[int32]bool, len(features))
	var needles, tagNeedles, missing int
	for _, f := range features {
		props := c.GeoJSON(f).Properties
		id, ok := props["id"].(int32)
		if !ok {
			t.Fatalf("missing id in %v", props)
		}
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
		if id%1500 == 0 {
			missing++
			for _, key := range []string{"c_byte", "c_float", "c_string", "c_ulong", "c_json", "c_tags"} {
				if _, ok := props[key]; ok {
					t.Fatalf("id %d keeps %s", id, key)
				}
			}
			continue
		}
		if props["c_string"] == "needle" {
			needles++
		}
		tags, err := corpusTags(props["c_tags"])
		if err != nil {
			t.Fatalf("id %d c_tags: %v", id, err)
		}
		for _, tag := range tags {
			if tag == "needle_tag" {
				tagNeedles++
			}
		}
		object, err := corpusObject(props["c_json"])
		if err != nil {
			t.Fatalf("id %d c_json: %v", id, err)
		}
		for _, key := range []string{"kind", "score", "flag", "tags", "label"} {
			if _, ok := object[key]; !ok {
				t.Fatalf("id %d c_json lacks %s", id, key)
			}
		}
		if _, ok := object["tags"].([]any); !ok {
			t.Fatalf("id %d c_json tags %T", id, object["tags"])
		}
		_, sparse := object["sparse"]
		if sparse != (id%3 == 0) {
			t.Fatalf("id %d sparse %v", id, sparse)
		}
	}
	if len(seen) != 30000 {
		t.Fatalf("unique ids %d", len(seen))
	}
	if missing != 20 {
		t.Fatalf("missing-attribute features %d", missing)
	}
	if needles != 10 {
		t.Fatalf("needle strings %d", needles)
	}
	if tagNeedles != 10 {
		t.Fatalf("needle_tag memberships %d", tagNeedles)
	}
}

func corpusTags(v any) ([]string, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("not a json string: %T", v)
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func corpusObject(v any) (map[string]any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("not a json string: %T", v)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}
