package spatial

import (
	"fmt"
	"math"
	"slices"

	"github.com/golang/geo/s2"
)

const PartitionLevel = 8

const maxCells = 1 << 14

func Key(bounds [4]float64) uint64 {
	lng := math.Max(-180, math.Min(180, (bounds[0]+bounds[2])/2))
	lat := math.Max(-90, math.Min(90, (bounds[1]+bounds[3])/2))
	return uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(lat, lng)))
}

func Partition(key uint64) uint64 {
	return uint64(s2.CellID(key).Parent(PartitionLevel))
}

func Range(cell uint64) (uint64, uint64) {
	c := s2.CellID(cell)
	return uint64(c.RangeMin()), uint64(c.RangeMax())
}

func Level(key uint64, level int) uint64 {
	return uint64(s2.CellID(key).Parent(level))
}

func Token(cell uint64) string {
	return s2.CellID(cell).ToToken()
}

func Cells(tokens []string) ([]uint64, error) {
	var cells []uint64
	add := func(id s2.CellID) error {
		cells = append(cells, uint64(id))
		if len(cells) > maxCells {
			return fmt.Errorf("cells expand to more than %d partitions", maxCells)
		}
		return nil
	}
	for _, token := range tokens {
		id := s2.CellIDFromToken(token)
		if !id.IsValid() {
			return nil, fmt.Errorf("invalid cell %q", token)
		}
		if id.Level() > PartitionLevel {
			return nil, fmt.Errorf("cell %q is finer than level %d", token, PartitionLevel)
		}
		if id.Level() == PartitionLevel {
			if err := add(id); err != nil {
				return nil, err
			}
			continue
		}
		for child := id.ChildBeginAtLevel(PartitionLevel); child != id.ChildEndAtLevel(PartitionLevel); child = child.Next() {
			if err := add(child); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(cells)
	return slices.Compact(cells), nil
}

func ToOrdered(key uint64) int64 {
	return int64(key ^ (1 << 63))
}

func FromOrdered(v int64) uint64 {
	return uint64(v) ^ (1 << 63)
}
