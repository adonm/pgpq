package geo

import "github.com/twpayne/go-geom/encoding/wkb"

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
