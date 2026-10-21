package spatial

import "testing"

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

func TestOrderedEncodingPreservesOrder(t *testing.T) {
	keys := []uint64{0, 1, 1 << 62, 1<<63 - 1, 1 << 63, 1<<63 + 1, 1<<64 - 1}
	for i := range keys {
		if i > 0 && ToOrdered(keys[i-1]) >= ToOrdered(keys[i]) {
			t.Fatalf("order not preserved between %x and %x", keys[i-1], keys[i])
		}
	}
}
