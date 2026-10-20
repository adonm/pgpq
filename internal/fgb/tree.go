package fgb

import (
	"encoding/binary"
	"math"
)

const NodeBytes = 40

type Item struct {
	BBox   [4]float64
	Offset uint64
}

func LevelCounts(count uint64, nodeSize uint16) []uint64 {
	if count == 0 || nodeSize < 2 {
		return nil
	}
	counts := make([]uint64, 1, 8)
	counts[0] = count
	for n := count; ; {
		n = (n + uint64(nodeSize) - 1) / uint64(nodeSize)
		counts = append(counts, n)
		if n == 1 {
			return counts
		}
	}
}

func LevelStarts(counts []uint64) []uint64 {
	starts := make([]uint64, len(counts))
	total := uint64(0)
	for _, count := range counts {
		total += count
	}
	for level := range counts {
		total -= counts[level]
		starts[level] = total
	}
	return starts
}

func AppendNode(dst []byte, bbox [4]float64, offset uint64) []byte {
	var raw [NodeBytes]byte
	for i := range bbox {
		binary.LittleEndian.PutUint64(raw[i*8:], math.Float64bits(bbox[i]))
	}
	binary.LittleEndian.PutUint64(raw[32:], offset)
	return append(dst, raw[:]...)
}

func NodeBBox(data []byte) [4]float64 {
	var bbox [4]float64
	for i := range bbox {
		bbox[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return bbox
}

func NodeOffset(data []byte) uint64 {
	return binary.LittleEndian.Uint64(data[32:NodeBytes])
}

func SetNodeOffset(data []byte, offset uint64) {
	binary.LittleEndian.PutUint64(data[32:NodeBytes], offset)
}

func Parents(children []byte, start uint64, nodeSize uint16) []byte {
	step := int(nodeSize) * NodeBytes
	out := make([]byte, 0, ((len(children)+step-1)/step)*NodeBytes)
	for at := 0; at < len(children); at += step {
		end := min(at+step, len(children))
		bbox := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
		for p := at; p+NodeBytes <= end; p += NodeBytes {
			child := NodeBBox(children[p:])
			bbox[0] = math.Min(bbox[0], child[0])
			bbox[1] = math.Min(bbox[1], child[1])
			bbox[2] = math.Max(bbox[2], child[2])
			bbox[3] = math.Max(bbox[3], child[3])
		}
		out = AppendNode(out, bbox, start+uint64(at/NodeBytes))
	}
	return out
}

func BuildIndex(items []Item, nodeSize uint16) []byte {
	if len(items) == 0 || nodeSize < 2 {
		return nil
	}
	counts := LevelCounts(uint64(len(items)), nodeSize)
	starts := LevelStarts(counts)
	levels := make([][]byte, len(counts))
	levels[0] = make([]byte, 0, len(items)*NodeBytes)
	for _, item := range items {
		levels[0] = AppendNode(levels[0], item.BBox, item.Offset)
	}
	for level := 0; level < len(counts)-1; level++ {
		levels[level+1] = Parents(levels[level], starts[level], nodeSize)
	}
	var out []byte
	for level := len(counts) - 1; level >= 0; level-- {
		out = append(out, levels[level]...)
	}
	return out
}

func Nodes(count uint64, nodeSize uint16) uint64 {
	var total uint64
	for _, level := range LevelCounts(count, nodeSize) {
		total += level
	}
	return total
}
