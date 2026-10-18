package fgb

import (
	"bytes"
	"testing"
)

func compareStreamedTree(t *testing.T, index []byte, total uint64) {
	t.Helper()
	counts := LevelCounts(total, NodeSize)
	starts := LevelStarts(counts)
	levels := make([][]byte, len(counts))
	levels[0] = index[len(index)-int(total)*NodeBytes:]
	for level := 0; level < len(counts)-1; level++ {
		levels[level+1] = Parents(levels[level], starts[level], NodeSize)
	}
	var built []byte
	for level := len(counts) - 1; level >= 0; level-- {
		built = append(built, levels[level]...)
	}
	if !bytes.Equal(index, built) {
		t.Fatalf("%d features: streamed tree is not the stored index", total)
	}
}

func TestStreamedTreeMatchesIndex(t *testing.T) {
	cases := [][]int{
		{2},
		{16},
		{17},
		{256},
		{257},
		{3, 5, 2},
		{600, 400},
		{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
	}
	for _, sizes := range cases {
		index, _ := testChunks(t, sizes...)
		var total uint64
		for _, size := range sizes {
			total += uint64(size)
		}
		compareStreamedTree(t, index, total)
	}
}

func TestStreamedTreeSingleLeaf(t *testing.T) {
	index, _, ok := Index([][]Leaf{{{BBox: [4]float64{1, 2, 3, 4}, Offset: 0}}}, []int64{8}, NodeSize)
	if !ok {
		t.Fatal("index build failed")
	}
	compareStreamedTree(t, index, 1)
}
