package fgb_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/spatial"
)

type countingReaderAt struct {
	r io.ReaderAt
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

type testFeature struct {
	id     string
	bounds [4]float64
	key    uint64
	input  chunk.InputFeature
}

func gridFeatures(n int, minX, minY, maxX, maxY float64) []testFeature {
	out := make([]testFeature, n)
	for i := range out {
		x := minX + (maxX-minX)*float64(i%60)/59
		y := minY + (maxY-minY)*float64(i/60)/math.Max(float64(n/60-1), 1)
		id := fmt.Sprintf("f-%05d", i)
		props := map[string]json.RawMessage{
			"id":   json.RawMessage(strconv.Quote(id)),
			"name": json.RawMessage(strconv.Quote(fmt.Sprintf("name-%d", i%7))),
		}
		out[i] = testFeature{
			id:     id,
			bounds: [4]float64{x, y, x, y},
			key:    spatial.Key([4]float64{x, y, x, y}),
			input:  testPoint([]chunk.Column{{Name: "id", Type: flattypes.ColumnTypeString}, {Name: "name", Type: flattypes.ColumnTypeString}}, props, x, y),
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key != out[j].key {
			return out[i].key < out[j].key
		}
		return out[i].id < out[j].id
	})
	return out
}

func encode(t *testing.T, features []testFeature) ([]byte, []fgb.FenceEntry) {
	t.Helper()
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
	}
	inputs := make([]chunk.InputFeature, len(features))
	keys := make([]uint64, len(features))
	ids := make([]string, len(features))
	for i, f := range features {
		inputs[i] = f.input
		keys[i] = f.key
		ids[i] = f.id
	}
	data, fence, err := chunk.EncodeCell("cell", testHeader(cols), inputs, keys, ids)
	if err != nil {
		t.Fatal(err)
	}
	return data, fence
}

func TestOrderedLayoutAndBboxRanges(t *testing.T) {
	features := gridFeatures(1200, 2.30, 48.80, 2.40, 48.90)
	data, fenceEntries := encode(t, features)
	fence, err := fgb.DecodeFence(fgb.EncodeFence(fenceEntries))
	if err != nil {
		t.Fatal(err)
	}
	if fence.Len() != (len(features)+fgb.FenceEvery-1)/fgb.FenceEvery {
		t.Fatalf("fence has %d entries", fence.Len())
	}
	counter := &countingReaderAt{r: bytes.NewReader(data)}
	layout, err := fgb.ReadLayout(counter, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Count() != len(features) {
		t.Fatalf("layout has %d rows, want %d", layout.Count(), len(features))
	}
	rows := layout.Rows()
	for i, row := range rows {
		if row.Index != i {
			t.Fatalf("row %d has index %d", i, row.Index)
		}
		offset, ok := layout.FeatureOffset(i)
		if !ok {
			t.Fatalf("row %d has no offset", i)
		}
		if i%fgb.FenceEvery == 0 {
			fenceOffset, ok := fence.RowOffset(i)
			if !ok || fenceOffset != uint64(offset) {
				t.Fatalf("row %d fence offset %d, want %d", i, fenceOffset, offset)
			}
		}
	}
	for i := 0; i < len(features); i += fgb.FenceEvery {
		if got := fence.StartRow(features[i].key, features[i].id); got > i {
			t.Fatalf("fence start for row %d is %d", i, got)
		}
	}

	bbox := [4]float64{2.305, 48.805, 2.315, 48.815}
	var want []string
	for _, f := range features {
		if f.bounds[0] >= bbox[0] && f.bounds[0] <= bbox[2] && f.bounds[1] >= bbox[1] && f.bounds[1] <= bbox[3] {
			want = append(want, f.id)
		}
	}
	hits := layout.Search(bbox[0], bbox[1], bbox[2], bbox[3])
	if len(hits) != len(want) {
		t.Fatalf("bbox hit %d rows, want %d", len(hits), len(want))
	}
	counter.n = 0
	found, err := layout.Features(counter, hits)
	if err != nil {
		t.Fatal(err)
	}
	cols := chunk.HeaderColumns(layout.Header())
	got := make([]string, len(found))
	for i, f := range found {
		var id string
		json.Unmarshal(chunk.DecodePropertyText(f.PropertiesBytes(), cols)["id"], &id)
		got[i] = id
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bbox features = %v, want %v", got, want)
	}
	if counter.n >= int64(len(data))/4 {
		t.Fatalf("bbox read %d of %d bytes", counter.n, len(data))
	}
	if len(hits) == 0 || len(hits) >= len(features)/2 {
		t.Fatalf("bbox selected %d of %d rows", len(hits), len(features))
	}
}

func TestFenceResumesEverySegment(t *testing.T) {
	features := gridFeatures(1100, 2.30, 48.80, 2.40, 48.90)
	_, fenceEntries := encode(t, features)
	fence, err := fgb.DecodeFence(fgb.EncodeFence(fenceEntries))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(features); i++ {
		start := fence.StartRow(features[i].key, features[i].id)
		if start > i {
			t.Fatalf("cursor at row %d starts at %d", i, start)
		}
		if i-start > 2*fgb.FenceEvery {
			t.Fatalf("cursor at row %d starts %d rows early", i, i-start)
		}
	}
	if fence.StartRow(math.MaxUint64, "") < len(features)-fgb.FenceEvery {
		t.Fatal("a key past the cell does not start in the last segment")
	}
}

func TestFenceHandlesEqualKeys(t *testing.T) {
	entries := []fgb.FenceEntry{
		{Key: 7, ID: "a", Offset: 100},
		{Key: 7, ID: "z", Offset: 200},
		{Key: 8, ID: "b", Offset: 300},
	}
	fence, err := fgb.DecodeFence(fgb.EncodeFence(entries))
	if err != nil {
		t.Fatal(err)
	}
	if got := fence.StartRow(7, "m"); got != 0 {
		t.Fatalf("a middle cursor in the first segment starts at %d", got)
	}
	if got := fence.StartRow(7, "zz"); got != fgb.FenceEvery {
		t.Fatalf("a cursor after the first segment starts at %d", got)
	}
	if got := fence.StartRow(8, "a"); got != fgb.FenceEvery {
		t.Fatalf("a cursor before a key change starts at %d", got)
	}
	if got := fence.StartRow(9, ""); got != 2*fgb.FenceEvery {
		t.Fatalf("a cursor after the fence starts at %d", got)
	}
}

func TestSingleFeatureIndexRoundTrip(t *testing.T) {
	features := gridFeatures(1, 2.30, 48.80, 2.40, 48.90)
	data, _ := encode(t, features)
	layout, err := fgb.ReadLayout(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Count() != 1 {
		t.Fatalf("layout count = %d", layout.Count())
	}
	reader, err := fgb.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
}
