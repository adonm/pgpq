package chunk

import "github.com/paulmach/orb"

func ToOrb(g *Geometry) orb.Geometry {
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
