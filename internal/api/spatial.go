package api

import (
	"github.com/paulmach/orb"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/geo"
)

type spatialWriter struct {
	base   featureSink
	shapes []geo.Shape
}

func (w *spatialWriter) add(g *chunk.Feature) bool {
	if !intersectsShapes(w.shapes, geo.ToOrb(g.Geometry)) {
		return true
	}
	return w.base.add(g)
}

func (w *spatialWriter) full() bool {
	return w.base.full()
}

func (w *spatialWriter) remaining() int {
	return w.base.remaining()
}

func spatialShapes(spatials []*cql.Spatial) []geo.Shape {
	out := make([]geo.Shape, 0, len(spatials))
	for _, spatial := range spatials {
		out = append(out, spatial.Shape)
	}
	return out
}

func intersectsShapes(shapes []geo.Shape, g orb.Geometry) bool {
	for _, shape := range shapes {
		if !shape.Intersects(g) {
			return false
		}
	}
	return true
}

func shapeBounds(shapes []geo.Shape) ([4]float64, bool) {
	var out [4]float64
	seen := false
	for _, shape := range shapes {
		b, ok := shape.Bounds()
		if !ok {
			continue
		}
		if !seen {
			out = b
			seen = true
			continue
		}
		out[0] = min(out[0], b[0])
		out[1] = min(out[1], b[1])
		out[2] = max(out[2], b[2])
		out[3] = max(out[3], b[3])
	}
	return out, seen
}

func mergeBBox(bbox *[4]float64, shapes []geo.Shape) (*[4]float64, bool) {
	if len(shapes) == 0 {
		return bbox, true
	}
	sb, ok := shapeBounds(shapes)
	if !ok {
		return nil, false
	}
	if bbox == nil {
		return &sb, true
	}
	out := [4]float64{
		max(bbox[0], sb[0]),
		max(bbox[1], sb[1]),
		min(bbox[2], sb[2]),
		min(bbox[3], sb[3]),
	}
	if out[0] > out[2] || out[1] > out[3] {
		return nil, false
	}
	return &out, true
}
