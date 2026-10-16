package chunk

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
)

func TestEncodeSearchRoundTrip(t *testing.T) {
	features := make([]InputFeature, 100)
	for i := range features {
		coords, err := json.Marshal([]float64{float64(i % 10), float64(i / 10)})
		if err != nil {
			t.Fatal(err)
		}
		features[i] = InputFeature{
			Geometry: RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"i":    json.RawMessage(strconv.Itoa(i)),
				"name": json.RawMessage(strconv.Quote("feature-" + strconv.Itoa(i))),
			},
		}
	}
	data, err := Encode("test", features)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Count(); got != 100 {
		t.Fatalf("count = %d, want 100", got)
	}
	env, ok := c.Envelope()
	if !ok {
		t.Fatal("missing envelope")
	}
	if env[0] != 0 || env[1] != 0 || env[2] != 9 || env[3] != 9 {
		t.Fatalf("envelope = %v, want [0 0 9 9]", env)
	}
	hits, err := c.Search(0, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("search hits = %d, want 4", len(hits))
	}
	g := c.GeoJSON(hits[0])
	if g.Type != "Feature" || g.Geometry == nil || g.Geometry.Type != "Point" {
		t.Fatalf("unexpected feature %+v", g)
	}
	if _, ok := g.Properties["i"].(int64); !ok {
		t.Fatalf("property i = %T, want int64", g.Properties["i"])
	}
	if _, ok := g.Properties["name"].(string); !ok {
		t.Fatalf("property name = %T, want string", g.Properties["name"])
	}
}

func TestFixturePolyLandmarks(t *testing.T) {
	data, err := os.ReadFile("../../testdata/poly_landmarks.fgb")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("fixture missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Count() == 0 {
		t.Fatal("fixture has no features")
	}
	env, ok := c.Envelope()
	if !ok {
		t.Fatal("fixture missing envelope")
	}
	hits, err := c.Search(env[0], env[1], env[2], env[3])
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("envelope search returned no features")
	}
	g := c.GeoJSON(hits[0])
	if g.Geometry == nil {
		t.Fatal("fixture feature missing geometry")
	}
}

func TestText(t *testing.T) {
	props := map[string]json.RawMessage{
		"name": json.RawMessage(`"Main Street"`),
		"kind": json.RawMessage(`"road"`),
		"rank": json.RawMessage(`7`),
		"gone": json.RawMessage(`null`),
	}
	if got, want := Text(props), "road Main Street 7"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}
