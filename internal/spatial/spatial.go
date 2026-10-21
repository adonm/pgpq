package spatial

import (
	"math"

	"github.com/golang/geo/s2"
)

func Key(bounds [4]float64) uint64 {
	lng := math.Max(-180, math.Min(180, (bounds[0]+bounds[2])/2))
	lat := math.Max(-90, math.Min(90, (bounds[1]+bounds[3])/2))
	return uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(lat, lng)))
}

func ToOrdered(key uint64) int64 {
	return int64(key ^ (1 << 63))
}
