package fgb

import (
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/index"
)

type Row struct {
	Index  int
	Offset uint64
}

type Layout struct {
	header     *flattypes.Header
	featuresAt int64
	size       int64
	tree       *index.PackedRTree
	index      []byte
	rows       []Row
	offsets    []uint64
}

func ReadLayout(r io.ReaderAt, size int64) (*Layout, error) {
	if size < 12 {
		return nil, errors.New("fgb: truncated file")
	}
	var head [12]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, err
	}
	for i := 0; i < len(magic); i++ {
		if i == 7 {
			continue
		}
		if head[i] != magic[i] {
			return nil, errors.New("fgb: not a flatgeobuf file")
		}
	}
	headerSize := int64(binary.LittleEndian.Uint32(head[8:]))
	if headerSize == 0 || headerSize > 64<<20 || 12+headerSize > size {
		return nil, errors.New("fgb: invalid header size")
	}
	buf := make([]byte, 4+headerSize)
	copy(buf, head[8:])
	if _, err := r.ReadAt(buf[4:], 12); err != nil {
		return nil, err
	}
	header := flattypes.GetSizePrefixedRootAsHeader(buf, 0)
	at := 12 + headerSize
	count := header.FeaturesCount()
	nodeSize := header.IndexNodeSize()
	if count > 0 && nodeSize >= 2 {
		length := int64(Nodes(count, nodeSize)) * NodeBytes
		if length <= 0 || at+length > size {
			return nil, errors.New("fgb: truncated index")
		}
		indexData := make([]byte, length)
		if _, err := r.ReadAt(indexData, at); err != nil {
			return nil, err
		}
		tree := index.NewPackedRTreeFromData(indexData, count, nodeSize, true)
		layout := &Layout{header: header, featuresAt: at + length, size: size, tree: tree, index: indexData}
		layout.loadRows()
		if len(layout.rows) != int(count) {
			return nil, errors.New("fgb: index does not cover every feature")
		}
		return layout, nil
	}
	return nil, errors.New("fgb: file has no R-tree index")
}

func (l *Layout) loadRows() {
	hits := l.tree.Search(math.Inf(-1), math.Inf(-1), math.Inf(1), math.Inf(1))
	l.rows = make([]Row, len(hits))
	l.offsets = make([]uint64, len(hits))
	for i, hit := range hits {
		l.rows[i] = Row{Index: int(hit.Index), Offset: hit.Offset}
		l.offsets[i] = hit.Offset
	}
}

func (l *Layout) Header() *flattypes.Header {
	return l.header
}

func (l *Layout) Count() int {
	return len(l.rows)
}

func (l *Layout) Rows() []Row {
	return l.rows
}

func (l *Layout) Search(minX, minY, maxX, maxY float64) []Row {
	hits := l.tree.Search(minX, minY, maxX, maxY)
	rows := make([]Row, len(hits))
	for i, hit := range hits {
		rows[i] = Row{Index: int(hit.Index), Offset: hit.Offset}
	}
	return rows
}

func (l *Layout) featureEnd(row int) int64 {
	if row+1 < len(l.offsets) {
		return l.featuresAt + int64(l.offsets[row+1])
	}
	return l.size
}

type RawFeature struct {
	Feature *flattypes.Feature
	Raw     []byte
}

func (l *Layout) Features(r io.ReaderAt, rows []Row) ([]*flattypes.Feature, error) {
	raw, err := l.ReadFeatures(r, rows)
	if err != nil {
		return nil, err
	}
	out := make([]*flattypes.Feature, len(raw))
	for i := range raw {
		out[i] = raw[i].Feature
	}
	return out, nil
}

func (l *Layout) ReadFeatures(r io.ReaderAt, rows []Row) ([]RawFeature, error) {
	out := make([]RawFeature, len(rows))
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].Index == rows[end-1].Index+1 {
			end++
		}
		first, last := rows[start].Index, rows[end-1].Index
		at := l.featuresAt + int64(l.offsets[first])
		stop := l.featureEnd(last)
		if at < l.featuresAt || stop < at || stop > l.size {
			return nil, errors.New("fgb: feature range outside the file")
		}
		buf := make([]byte, stop-at)
		if _, err := r.ReadAt(buf, at); err != nil {
			return nil, err
		}
		off := 0
		for i := start; i < end; i++ {
			if off+4 > len(buf) {
				return nil, errors.New("fgb: truncated feature")
			}
			length := int(binary.LittleEndian.Uint32(buf[off:]))
			if length <= 0 || off+4+length > len(buf) {
				return nil, errors.New("fgb: invalid feature length")
			}
			raw := buf[off : off+4+length]
			out[i] = RawFeature{Feature: flattypes.GetSizePrefixedRootAsFeature(raw, 0), Raw: raw}
			off += 4 + length
		}
		start = end
	}
	return out, nil
}

func (l *Layout) Feature(r io.ReaderAt, row Row) (*flattypes.Feature, error) {
	features, err := l.Features(r, []Row{row})
	if err != nil {
		return nil, err
	}
	return features[0], nil
}

func (l *Layout) ReadFeature(r io.ReaderAt, row Row) (RawFeature, error) {
	features, err := l.ReadFeatures(r, []Row{row})
	if err != nil {
		return RawFeature{}, err
	}
	return features[0], nil
}

func (l *Layout) FeatureOffset(row int) (int64, bool) {
	if row < 0 || row >= len(l.offsets) {
		return 0, false
	}
	return l.featuresAt + int64(l.offsets[row]), true
}

func (l *Layout) RootBBox() ([4]float64, bool) {
	if len(l.index) < NodeBytes {
		return [4]float64{}, false
	}
	return NodeBBox(l.index[:NodeBytes]), true
}

func (l *Layout) IndexBytes() []byte {
	return l.index
}
