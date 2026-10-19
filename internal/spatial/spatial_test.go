package spatial

import (
	"testing"

	"github.com/golang/geo/s2"
)

func TestPartitionIsLevelEight(t *testing.T) {
	for _, p := range [][2]float64{{48.85, 2.35}, {23.78, 90.40}, {-33.9, 151.2}, {0, 0}} {
		key := Key([4]float64{p[1], p[0], p[1], p[0]})
		cell := Partition(key)
		if got := s2.CellID(cell).Level(); got != PartitionLevel {
			t.Fatalf("partition of %v is level %d", p, got)
		}
		if !s2.CellID(cell).Contains(s2.CellID(key)) {
			t.Fatalf("partition of %v does not contain its key", p)
		}
	}
}

func TestKeyUsesBoundsCenter(t *testing.T) {
	point := Key([4]float64{2.35, 48.85, 2.35, 48.85})
	box := Key([4]float64{2.0, 48.5, 2.7, 49.2})
	if point == 0 {
		t.Fatal("zero key")
	}
	if box != point {
		t.Fatalf("box key %x differs from its centre %x", box, point)
	}
}

func TestCellsExpandCoarseTokens(t *testing.T) {
	level6 := s2.CellIDFromFace(0).Children()[0].Children()[0].Children()[0].Children()[0].Children()[0].ChildBegin()
	coarse := level6.Parent(6)
	cells, err := Cells([]string{coarse.ToToken()})
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 16 {
		t.Fatalf("level-6 cell expands to %d partitions, want 16", len(cells))
	}
	for _, c := range cells {
		if s2.CellID(c).Level() != PartitionLevel || s2.CellID(c).Parent(6) != coarse {
			t.Fatalf("expanded cell %x is outside %v", c, coarse)
		}
	}
}

func TestCellsRejectFinerAndInvalid(t *testing.T) {
	fine := s2.CellIDFromLatLng(s2.LatLngFromDegrees(48.85, 2.35)).Parent(9)
	if _, err := Cells([]string{fine.ToToken()}); err == nil {
		t.Fatal("level-9 cell accepted")
	}
	if _, err := Cells([]string{"not-a-cell"}); err == nil {
		t.Fatal("invalid token accepted")
	}
	if _, err := Cells([]string{s2.CellIDFromFace(0).ToToken()}); err == nil {
		t.Fatal("a whole face expands past the cell cap and was accepted")
	}
}

func TestOrderedEncodingPreservesOrder(t *testing.T) {
	keys := []uint64{0, 1, 1 << 62, 1<<63 - 1, 1 << 63, 1<<63 + 1, 1<<64 - 1}
	for i := range keys {
		if FromOrdered(ToOrdered(keys[i])) != keys[i] {
			t.Fatalf("round trip failed for %x", keys[i])
		}
		if i > 0 && ToOrdered(keys[i-1]) >= ToOrdered(keys[i]) {
			t.Fatalf("order not preserved between %x and %x", keys[i-1], keys[i])
		}
	}
}
