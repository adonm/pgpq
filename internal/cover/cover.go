package cover

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/geo/r1"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

func Chunk(minX, minY, maxX, maxY float64) string {
	return multirange(rect(minX, minY, maxX, maxY), 8)
}

func Query(minX, minY, maxX, maxY float64) string {
	return multirange(rect(minX, minY, maxX, maxY), 512)
}

type interval struct {
	lo, hi int64
}

func multirange(r s2.Rect, maxCells int) string {
	coverer := &s2.RegionCoverer{MaxCells: maxCells, MinLevel: 0, MaxLevel: 30}
	cells := coverer.Covering(r)
	intervals := make([]interval, 0, len(cells))
	for _, cell := range cells {
		intervals = append(intervals, interval{ordered(uint64(cell.RangeMin())), ordered(uint64(cell.RangeMax()))})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].lo < intervals[j].lo })
	var merged []interval
	for _, iv := range intervals {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if iv.lo <= last.hi || (last.hi < math.MaxInt64 && iv.lo == last.hi+1) {
				if iv.hi > last.hi {
					last.hi = iv.hi
				}
				continue
			}
		}
		merged = append(merged, iv)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, iv := range merged {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		b.WriteString(strconv.FormatInt(iv.lo, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(iv.hi, 10))
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.String()
}

func rect(minX, minY, maxX, maxY float64) s2.Rect {
	if minX > maxX {
		minX, maxX = maxX, minX
	}
	if minY > maxY {
		minY, maxY = maxY, minY
	}
	lat := r1.Interval{
		Lo: radians(clamp(minY, -90, 90)),
		Hi: radians(clamp(maxY, -90, 90)),
	}
	lng := s1.Interval{
		Lo: radians(clamp(minX, -180, 180)),
		Hi: radians(clamp(maxX, -180, 180)),
	}
	if minX <= -180 && maxX >= 180 {
		lng = s1.Interval{Lo: -math.Pi, Hi: math.Pi}
	}
	return s2.Rect{Lat: lat, Lng: lng}
}

func radians(deg float64) float64 {
	return deg * math.Pi / 180
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ordered(u uint64) int64 {
	return int64(u ^ 1<<63)
}
