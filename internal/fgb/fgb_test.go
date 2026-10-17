package fgb

import (
	"encoding/json"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/index"

	"github.com/adonm/postfgb/internal/chunk"
)

func testChunks(t *testing.T, sizes ...int) ([]byte, []byte) {
	t.Helper()
	cols := []chunk.Column{{Name: "name", Type: flattypes.ColumnTypeString}}
	chunks := make([][]byte, len(sizes))
	leaves := make([][]Leaf, len(sizes))
	lengths := make([]int64, len(sizes))
	total := 0
	for i, size := range sizes {
		features := make([]chunk.InputFeature, size)
		for j := range features {
			coords, err := json.Marshal([]float64{7.4 + float64(j)*0.001, 43.7 + float64(i)*0.001})
			if err != nil {
				t.Fatal(err)
			}
			features[j] = chunk.InputFeature{
				Geometry:   chunk.RawGeometry{Type: "Point", Coordinates: coords},
				Properties: map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
			}
		}
		data, err := chunk.EncodeCols("t", cols, features)
		if err != nil {
			t.Fatal(err)
		}
		chunks[i] = data
		total += size
	}
	var indexBytes []byte
	for i, data := range chunks {
		c, err := chunk.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		_, featureBytes, err := Regions(data, c.Header())
		if err != nil {
			t.Fatal(err)
		}
		lengths[i] = int64(featureBytes)
		leaves[i] = LeavesOf(data, c.Header())
		if len(leaves[i]) != sizes[i] {
			t.Fatalf("chunk %d: %d leaves for %d features", i, len(leaves[i]), sizes[i])
		}
	}
	var envelope [4]float64
	ok := false
	indexBytes, envelope, ok = Index(leaves, lengths, NodeSize)
	if !ok {
		t.Fatal("index build failed")
	}
	header, ok := Header("t", headerBlobOf(t, chunks[0]), uint64(total), envelope)
	if !ok {
		t.Fatal("header build failed")
	}
	if len(indexBytes) != int(Nodes(uint64(total), NodeSize))*40 {
		t.Fatalf("index size = %d for %d features", len(indexBytes), total)
	}
	parsed := flattypes.GetSizePrefixedRootAsHeader(header, 0)
	if parsed == nil || parsed.FeaturesCount() != uint64(total) || parsed.IndexNodeSize() != NodeSize {
		t.Fatalf("header = %+v", parsed)
	}
	tree := index.NewPackedRTreeFromData(indexBytes, uint64(total), NodeSize, false)
	hits := tree.Search(envelope[0]-1, envelope[1]-1, envelope[2]+1, envelope[3]+1)
	if len(hits) != total {
		t.Fatalf("search returned %d of %d features", len(hits), total)
	}
	offsets := map[uint64]bool{}
	var base int64
	for i, chunkLeaves := range leaves {
		for _, leaf := range chunkLeaves {
			offsets[uint64(base)+leaf.Offset] = true
		}
		base += lengths[i]
	}
	for _, hit := range hits {
		if !offsets[hit.Offset] {
			t.Fatalf("hit offset %d is not a feature start", hit.Offset)
		}
	}
	return indexBytes, header
}

func headerBlobOf(t *testing.T, data []byte) []byte {
	t.Helper()
	c, err := chunk.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	blob := HeaderBlobOf(data, c.Header())
	if blob == nil {
		t.Fatal("no header blob")
	}
	return blob
}

func TestIndexRebasesChunkLeaves(t *testing.T) {
	testChunks(t, 3, 5, 2)
}

func TestIndexOverManyFeatures(t *testing.T) {
	testChunks(t, 600, 400)
}
