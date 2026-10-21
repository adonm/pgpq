package geo

import (
	"encoding/json"

	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkb"
)

func GeoJSON(f Feature) (map[string]any, error) {
	g, err := wkb.Unmarshal(f.Geometry)
	if err != nil {
		return nil, err
	}
	raw, err := geojson.Marshal(g)
	if err != nil {
		return nil, err
	}
	var geometry json.RawMessage = raw
	props := f.Props
	if props == nil {
		props = map[string]any{}
	}
	return map[string]any{"type": "Feature", "id": f.ID, "geometry": geometry, "properties": props}, nil
}

func Bounds(g []byte) ([4]float64, bool) {
	geom, err := wkb.Unmarshal(g)
	if err != nil {
		return [4]float64{}, false
	}
	b := geom.Bounds()
	if b.IsEmpty() {
		return [4]float64{}, false
	}
	return [4]float64{b.Min(0), b.Min(1), b.Max(0), b.Max(1)}, true
}
