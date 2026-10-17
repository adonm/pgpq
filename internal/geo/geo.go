package geo

import (
	"github.com/adonm/postfgb/internal/chunk"

	"math"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
)

type Point struct {
	X, Y float64
}

type Ring []Point

type Polygon []Ring

type Shape []Polygon

func (s Shape) Bounds() ([4]float64, bool) {
	var b [4]float64
	seen := false
	for _, poly := range s {
		for _, ring := range poly {
			for _, p := range ring {
				if !seen {
					b = [4]float64{p.X, p.Y, p.X, p.Y}
					seen = true
					continue
				}
				b[0] = math.Min(b[0], p.X)
				b[1] = math.Min(b[1], p.Y)
				b[2] = math.Max(b[2], p.X)
				b[3] = math.Max(b[3], p.Y)
			}
		}
	}
	return b, seen
}

func (s Shape) Intersects(g orb.Geometry) bool {
	switch v := g.(type) {
	case orb.Point:
		return s.containsPoint(v[0], v[1])
	case orb.MultiPoint:
		for _, p := range v {
			if s.containsPoint(p[0], p[1]) {
				return true
			}
		}
	case orb.LineString:
		return s.intersectsLine(v)
	case orb.MultiLineString:
		for _, line := range v {
			if s.intersectsLine(line) {
				return true
			}
		}
	case orb.Polygon:
		return s.intersectsPolygon(v)
	case orb.MultiPolygon:
		for _, poly := range v {
			if s.intersectsPolygon(poly) {
				return true
			}
		}
	case orb.Collection:
		for _, part := range v {
			if s.Intersects(part) {
				return true
			}
		}
	}
	return false
}

func (s Shape) containsPoint(x, y float64) bool {
	for _, poly := range s {
		if polyContains(poly, x, y) {
			return true
		}
	}
	return false
}

func polyContains(poly Polygon, x, y float64) bool {
	if len(poly) == 0 {
		return false
	}
	if ringBoundary(poly[0], x, y) {
		return true
	}
	if !ringContains(poly[0], x, y) {
		return false
	}
	for _, hole := range poly[1:] {
		if ringBoundary(hole, x, y) {
			return true
		}
		if ringContains(hole, x, y) {
			return false
		}
	}
	return true
}

func ringContains(ring Ring, x, y float64) bool {
	n := len(ring)
	if n < 3 {
		return false
	}
	inside := false
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		xi, yi := ring[i].X, ring[i].Y
		xj, yj := ring[j].X, ring[j].Y
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			inside = !inside
		}
	}
	return inside
}

func ringBoundary(ring Ring, x, y float64) bool {
	n := len(ring)
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		if onSegment(ring[i].X, ring[i].Y, ring[j].X, ring[j].Y, x, y) {
			return true
		}
	}
	return false
}

func (s Shape) intersectsLine(line orb.LineString) bool {
	for i := 0; i+1 < len(line); i++ {
		if s.intersectsSegment(line[i][0], line[i][1], line[i+1][0], line[i+1][1]) {
			return true
		}
	}
	return false
}

func (s Shape) intersectsSegment(ax, ay, bx, by float64) bool {
	if s.containsPoint(ax, ay) || s.containsPoint(bx, by) {
		return true
	}
	for _, poly := range s {
		for _, ring := range poly {
			n := len(ring)
			for i := 0; i < n; i++ {
				j := (i + 1) % n
				if segmentsIntersect(ax, ay, bx, by, ring[i].X, ring[i].Y, ring[j].X, ring[j].Y) {
					return true
				}
			}
		}
	}
	return false
}

func (s Shape) intersectsPolygon(poly orb.Polygon) bool {
	subject := contourPolygon(poly)
	if len(subject) == 0 {
		return false
	}
	for _, part := range s {
		clipping := make(polyclip.Polygon, 0, len(part))
		for i, ring := range part {
			contour := make(polyclip.Contour, 0, len(ring))
			for _, p := range ring {
				contour = append(contour, polyclip.Point{X: p.X, Y: p.Y})
			}
			if len(contour) > 0 && isCCW(contour) != (i == 0) {
				reverseContour(contour)
			}
			clipping = append(clipping, contour)
		}
		if len(subject.Construct(polyclip.INTERSECTION, clipping)) > 0 {
			return true
		}
	}
	return false
}

func contourPolygon(poly orb.Polygon) polyclip.Polygon {
	out := make(polyclip.Polygon, 0, len(poly))
	for i, ring := range poly {
		contour := make(polyclip.Contour, 0, len(ring))
		for _, p := range ring {
			contour = append(contour, polyclip.Point{X: p[0], Y: p[1]})
		}
		if len(contour) > 0 && isCCW(contour) != (i == 0) {
			reverseContour(contour)
		}
		out = append(out, contour)
	}
	return out
}

func isCCW(c polyclip.Contour) bool {
	area := 0.0
	n := len(c)
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		area += (c[j].X - c[i].X) * (c[j].Y + c[i].Y)
	}
	return area < 0
}

func reverseContour(c polyclip.Contour) {
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
}

func onSegment(ax, ay, bx, by, px, py float64) bool {
	return orient(ax, ay, bx, by, px, py) == 0 &&
		px >= math.Min(ax, bx) && px <= math.Max(ax, bx) &&
		py >= math.Min(ay, by) && py <= math.Max(ay, by)
}

func orient(ax, ay, bx, by, cx, cy float64) int {
	v := (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

func segmentsIntersect(ax, ay, bx, by, cx, cy, dx, dy float64) bool {
	o1 := orient(ax, ay, bx, by, cx, cy)
	o2 := orient(ax, ay, bx, by, dx, dy)
	o3 := orient(cx, cy, dx, dy, ax, ay)
	o4 := orient(cx, cy, dx, dy, bx, by)
	if o1 != o2 && o3 != o4 {
		return true
	}
	switch {
	case o1 == 0 && onSegment(ax, ay, bx, by, cx, cy):
		return true
	case o2 == 0 && onSegment(ax, ay, bx, by, dx, dy):
		return true
	case o3 == 0 && onSegment(cx, cy, dx, dy, ax, ay):
		return true
	case o4 == 0 && onSegment(cx, cy, dx, dy, bx, by):
		return true
	}
	return false
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
