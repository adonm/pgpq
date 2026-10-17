package chunk

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestGeometryRoundTrip(t *testing.T) {
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
		features := []InputFeature{{
			Geometry:   RawGeometry{Type: input.Type, Coordinates: input.Coordinates},
			Properties: map[string]json.RawMessage{"i": json.RawMessage(strconv.Itoa(1))},
		}}
		data, err := Encode("t", features)
		if err != nil {
			t.Fatalf("%s: encode: %v", input.Type, err)
		}
		c, err := Open(data)
		if err != nil {
			t.Fatalf("%s: open: %v", input.Type, err)
		}
		all, err := c.SearchAll()
		if err != nil || len(all) != 1 {
			t.Fatalf("%s: search: %v (%d)", input.Type, err, len(all))
		}
		output, err := json.Marshal(c.GeoJSON(all[0]).Geometry)
		if err != nil {
			t.Fatalf("%s: marshal: %v", input.Type, err)
		}
		if normalize(t, geometry) != normalize(t, string(output)) {
			t.Fatalf("%s round trip:\n in: %s\nout: %s", input.Type, geometry, output)
		}
	}
}

func normalize(t *testing.T, value string) string {
	var v any
	if err := json.Unmarshal([]byte(value), &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
