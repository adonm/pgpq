package chunk

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

func TestParseFlatRoundTrip(t *testing.T) {
	cases := []string{
		`{"type":"Point","coordinates":[3,4]}`,
		`{"type":"MultiPoint","coordinates":[[1,1],[2,2]]}`,
		`{"type":"LineString","coordinates":[[0,0],[5,5],[10,0]]}`,
		`{"type":"MultiLineString","coordinates":[[[0,0],[1,1]],[[2,2],[3,3]]]}`,
		`{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]],[[2,2],[4,2],[4,4],[2,4],[2,2]]]}`,
		`{"type":"MultiPolygon","coordinates":[[[[0,0],[1,0],[1,1],[0,1],[0,0]]],[[[5,5],[6,5],[6,6],[5,6],[5,5]]]]}`,
	}
	for _, geometry := range cases {
		var input struct {
			Type        string          `json:"type"`
			Coordinates json.RawMessage `json:"coordinates"`
		}
		if err := json.Unmarshal([]byte(geometry), &input); err != nil {
			t.Fatal(err)
		}
		props := map[string]json.RawMessage{"i": json.RawMessage(strconv.Itoa(1))}
		source := []InputFeature{{Geometry: RawGeometry{Type: input.Type, Coordinates: input.Coordinates}, Properties: props}}
		data, err := Encode("t", source)
		if err != nil {
			t.Fatalf("%s: encode: %v", input.Type, err)
		}
		c, err := Open(data)
		if err != nil {
			t.Fatalf("%s: open: %v", input.Type, err)
		}
		all, err := c.SearchAll()
		if err != nil {
			t.Fatalf("%s: search: %v", input.Type, err)
		}
		shape, err := ParseFlat(c.header, all[0])
		if err != nil {
			t.Fatalf("%s: parse flat: %v", input.Type, err)
		}
		again, err := Encode("t", []InputFeature{{Parsed: shape, Properties: props}})
		if err != nil {
			t.Fatalf("%s: re-encode: %v", input.Type, err)
		}
		if !bytes.Equal(data, again) {
			t.Fatalf("%s: parsed geometry does not reproduce the encoded bytes", input.Type)
		}
	}
}

func TestDecodePropertyTextMatchesValues(t *testing.T) {
	cols := []Column{
		{Name: "c_bool", Type: flattypes.ColumnTypeBool},
		{Name: "c_byte", Type: flattypes.ColumnTypeByte},
		{Name: "c_short", Type: flattypes.ColumnTypeShort},
		{Name: "c_int", Type: flattypes.ColumnTypeInt},
		{Name: "c_long", Type: flattypes.ColumnTypeLong},
		{Name: "c_double", Type: flattypes.ColumnTypeDouble},
		{Name: "c_string", Type: flattypes.ColumnTypeString},
		{Name: "c_datetime", Type: flattypes.ColumnTypeDateTime},
		{Name: "c_json", Type: flattypes.ColumnTypeJson},
	}
	features := make([]InputFeature, 4)
	for i := range features {
		coords, _ := json.Marshal([]float64{float64(i), float64(i)})
		features[i] = InputFeature{
			Geometry: RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"c_bool":     json.RawMessage("true"),
				"c_byte":     json.RawMessage(strconv.Itoa(i - 2)),
				"c_short":    json.RawMessage(strconv.Itoa(1000 + i)),
				"c_int":      json.RawMessage(strconv.Itoa(100000 + i)),
				"c_long":     json.RawMessage("9007199254740993"),
				"c_double":   json.RawMessage(strconv.FormatFloat(float64(i)+0.5, 'g', -1, 64)),
				"c_string":   json.RawMessage(strconv.Quote("va\"lue\n" + strconv.Itoa(i))),
				"c_datetime": json.RawMessage(`"2026-01-02T03:04:05Z"`),
				"c_json":     json.RawMessage(`[1,{"a":2}]`),
			},
		}
	}
	data, err := EncodeCols("t", cols, features)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	gotCols := c.Columns()
	for _, f := range all {
		raw := f.PropertiesBytes()
		got := DecodePropertyText(raw, gotCols)
		want := decodeProperties(raw, gotCols)
		for name, wantValue := range want {
			encoded, err := json.Marshal(wantValue)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(encoded, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got[name], &b); err != nil {
				t.Fatalf("%s: %s is not JSON: %v", name, got[name], err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("%s: text decode %v differs from value decode %v", name, b, a)
			}
		}
	}
}
