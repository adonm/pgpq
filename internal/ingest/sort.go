package ingest

import (
	"sort"

	"github.com/flatgeobuf/flatgeobuf/src/go/index"

	"github.com/adonm/postfgb/internal/chunk"
)

func sortWindow(features []chunk.InputFeature) {
	if len(features) < 2 {
		return
	}
	keys := make([]uint32, len(features))
	for i := range features {
		keys[i] = hilbertKey(features[i])
	}
	order := make([]int, len(features))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return keys[order[a]] < keys[order[b]] })
	sorted := make([]chunk.InputFeature, len(features))
	for i, j := range order {
		sorted[i] = features[j]
	}
	copy(features, sorted)
}

func hilbertKey(f chunk.InputFeature) uint32 {
	bounds, ok := chunk.Bounds(&f.Geometry)
	if !ok {
		return 0
	}
	x := (bounds[0] + bounds[2]) / 2
	y := (bounds[1] + bounds[3]) / 2
	if x < -180 {
		x = -180
	}
	if x > 180 {
		x = 180
	}
	if y < -90 {
		y = -90
	}
	if y > 90 {
		y = 90
	}
	hx := uint32((x + 180) / 360 * 0xFFFF)
	hy := uint32((y + 90) / 180 * 0xFFFF)
	return index.Hilbert(hx, hy)
}
