package chunk

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

func TestScalarTypesRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		typ  flattypes.ColumnType
		in   string
		want any
		json string
	}{
		{"byte", flattypes.ColumnTypeByte, "-128", int8(math.MinInt8), "-128"},
		{"byte max", flattypes.ColumnTypeByte, "127", int8(math.MaxInt8), "127"},
		{"ubyte", flattypes.ColumnTypeUByte, "255", uint8(math.MaxUint8), "255"},
		{"bool true", flattypes.ColumnTypeBool, "true", true, "true"},
		{"bool false", flattypes.ColumnTypeBool, "false", false, "false"},
		{"bool as string", flattypes.ColumnTypeBool, `"false"`, false, "false"},
		{"short", flattypes.ColumnTypeShort, "-32768", int16(math.MinInt16), "-32768"},
		{"ushort", flattypes.ColumnTypeUShort, "65535", uint16(math.MaxUint16), "65535"},
		{"int", flattypes.ColumnTypeInt, "-2147483648", int32(math.MinInt32), "-2147483648"},
		{"uint", flattypes.ColumnTypeUInt, "4294967295", uint32(math.MaxUint32), "4294967295"},
		{"long", flattypes.ColumnTypeLong, "-9223372036854775808", int64(math.MinInt64), "-9223372036854775808"},
		{"long beyond float64", flattypes.ColumnTypeLong, "9007199254740993", int64(9007199254740993), "9007199254740993"},
		{"long zero", flattypes.ColumnTypeLong, "0", int64(0), "0"},
		{"ulong", flattypes.ColumnTypeULong, "18446744073709551615", uint64(math.MaxUint64), "18446744073709551615"},
		{"float keeps float32", flattypes.ColumnTypeFloat, "0.1", float32(0.1), "0.1"},
		{"double", flattypes.ColumnTypeDouble, "2.718281828459045", 2.718281828459045, "2.718281828459045"},
		{"double from an integer", flattypes.ColumnTypeDouble, "3", 3.0, "3"},
		{"string", flattypes.ColumnTypeString, `"h\u00e9llo, \"x\""`, `héllo, "x"`, `"héllo, \"x\""`},
		{"empty string", flattypes.ColumnTypeString, `""`, "", `""`},
		{"datetime", flattypes.ColumnTypeDateTime, `"2026-10-18T06:00:00Z"`, "2026-10-18T06:00:00Z", `"2026-10-18T06:00:00Z"`},
		{"binary", flattypes.ColumnTypeBinary, `"AAEC/w=="`, []byte{0, 1, 2, 255}, `"AAEC/w=="`},
		{"empty binary", flattypes.ColumnTypeBinary, `""`, []byte{}, `""`},
		{"json", flattypes.ColumnTypeJson, `[1,2]`, []any{1.0, 2.0}, `[1,2]`},
		{"null is absent", flattypes.ColumnTypeLong, "null", nil, ""},
		{"fraction is absent in an integer column", flattypes.ColumnTypeLong, "1.5", nil, ""},
		{"out of range is absent", flattypes.ColumnTypeUByte, "256", nil, ""},
		{"number is absent in a bool column", flattypes.ColumnTypeBool, "1", nil, ""},
	}
	cols := make([]Column, len(cases))
	props := map[string]json.RawMessage{}
	for i, tc := range cases {
		cols[i] = Column{Name: fmt.Sprintf("c%02d", i), Type: tc.typ}
		props[cols[i].Name] = json.RawMessage(tc.in)
	}
	data, err := EncodeCols("t", cols, []InputFeature{{
		Geometry:   RawGeometry{Type: "Point", Coordinates: json.RawMessage("[1,2]")},
		Properties: props,
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.SearchAll()
	if err != nil || len(all) != 1 {
		t.Fatalf("search: %v (%d features)", err, len(all))
	}
	decoded := c.GeoJSON(all[0]).Properties
	for i, tc := range cases {
		got, ok := decoded[cols[i].Name]
		if tc.want == nil {
			if ok {
				t.Fatalf("%s: got %#v, want no value", tc.name, got)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %#v (%T), want %#v", tc.name, got, got, tc.want)
		}
		out, err := json.Marshal(got)
		if err != nil || string(out) != tc.json {
			t.Fatalf("%s: JSON %s, want %s", tc.name, out, tc.json)
		}
	}
}
