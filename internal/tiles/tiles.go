package tiles

import (
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/simplify"

	"github.com/adonm/postfgb/internal/chunk"
)

func Bytes(name string, collection *orbgeojson.FeatureCollection, tile maptile.Tile) ([]byte, bool, error) {
	layer := mvt.NewLayer(name, collection)
	layer.ProjectToTile(tile)
	layer.Clip(mvt.MapboxGLDefaultExtentBound)
	layer.Simplify(simplify.DouglasPeucker(1.0))
	layer.RemoveEmpty(1.0, 2.0)
	if len(layer.Features) == 0 {
		return nil, true, nil
	}
	data, err := mvt.MarshalGzipped(mvt.Layers{layer})
	if err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func BoundsFor(b [4]float64, z uint8) (uint32, uint32, uint32, uint32) {
	nw := maptile.At(orb.Point{b[0], b[3]}, maptile.Zoom(z))
	se := maptile.At(orb.Point{b[2], b[1]}, maptile.Zoom(z))
	last := uint32(1)<<z - 1
	clamp := func(v uint32) uint32 {
		if v > last {
			return last
		}
		return v
	}
	x0, x1 := clamp(nw.X), clamp(se.X)
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	y0, y1 := clamp(nw.Y), clamp(se.Y)
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	return x0, x1, y0, y1
}

func ToOrb(g *chunk.Geometry) orb.Geometry {
	if g == nil {
		return nil
	}
	switch g.Type {
	case "Point":
		pts, ok := g.Coordinates.([]float64)
		if !ok || len(pts) < 2 {
			return nil
		}
		return orb.Point{pts[0], pts[1]}
	case "LineString":
		pts, ok := g.Coordinates.([][]float64)
		if !ok {
			return nil
		}
		return orbLine(pts)
	case "Polygon":
		rs, ok := g.Coordinates.([][][]float64)
		if !ok {
			return nil
		}
		return orbPoly(rs)
	case "MultiPoint":
		pts, ok := g.Coordinates.([][]float64)
		if !ok {
			return nil
		}
		mp := make(orb.MultiPoint, 0, len(pts))
		for _, p := range pts {
			if len(p) < 2 {
				continue
			}
			mp = append(mp, orb.Point{p[0], p[1]})
		}
		return mp
	case "MultiLineString":
		ls, ok := g.Coordinates.([][][]float64)
		if !ok {
			return nil
		}
		ml := make(orb.MultiLineString, 0, len(ls))
		for _, l := range ls {
			ml = append(ml, orbLine(l))
		}
		return ml
	case "MultiPolygon":
		ps, ok := g.Coordinates.([][][][]float64)
		if !ok {
			return nil
		}
		mp := make(orb.MultiPolygon, 0, len(ps))
		for _, p := range ps {
			mp = append(mp, orbPoly(p))
		}
		return mp
	case "GeometryCollection":
		var parts []orb.Geometry
		for i := range g.Geometries {
			if part := ToOrb(&g.Geometries[i]); part != nil {
				parts = append(parts, part)
			}
		}
		return orb.Collection(parts)
	}
	return nil
}

func orbLine(pts [][]float64) orb.LineString {
	line := make(orb.LineString, 0, len(pts))
	for _, p := range pts {
		if len(p) < 2 {
			continue
		}
		line = append(line, orb.Point{p[0], p[1]})
	}
	return line
}

func orbPoly(rs [][][]float64) orb.Polygon {
	poly := make(orb.Polygon, 0, len(rs))
	for _, r := range rs {
		poly = append(poly, orb.Ring(orbLine(r)))
	}
	return poly
}
