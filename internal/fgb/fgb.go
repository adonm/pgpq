package fgb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	"github.com/flatgeobuf/flatgeobuf/src/go/index"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

var magic = [8]byte{0x66, 0x67, 0x62, 0x03, 0x66, 0x67, 0x62, 0x00}

type Reader struct {
	in     io.Reader
	header *flattypes.Header
}

func NewReader(r io.Reader) (*Reader, error) {
	in := bufio.NewReaderSize(r, 1<<20)
	var mb [8]byte
	if _, err := io.ReadFull(in, mb[:]); err != nil {
		return nil, errors.New("fgb: missing magic bytes")
	}
	for i := 0; i < len(magic); i++ {
		if i == 7 {
			continue
		}
		if mb[i] != magic[i] {
			return nil, errors.New("fgb: not a flatgeobuf stream")
		}
	}
	var sizeBuf [4]byte
	if _, err := io.ReadFull(in, sizeBuf[:]); err != nil {
		return nil, errors.New("fgb: truncated header")
	}
	size := binary.LittleEndian.Uint32(sizeBuf[:])
	if size == 0 || size > 64<<20 {
		return nil, errors.New("fgb: invalid header size")
	}
	buf := make([]byte, size+4)
	copy(buf, sizeBuf[:])
	if _, err := io.ReadFull(in, buf[4:]); err != nil {
		return nil, errors.New("fgb: truncated header")
	}
	header := flattypes.GetSizePrefixedRootAsHeader(buf, 0)
	if skip := treeSize(header.FeaturesCount(), header.IndexNodeSize()); skip > 0 {
		if _, err := io.CopyN(io.Discard, in, int64(skip)); err != nil {
			return nil, errors.New("fgb: truncated index")
		}
	}
	return &Reader{in: in, header: header}, nil
}

func (r *Reader) Header() *flattypes.Header {
	return r.header
}

func (r *Reader) Next() (*flattypes.Feature, error) {
	var sizeBuf [4]byte
	if _, err := io.ReadFull(r.in, sizeBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, errors.New("fgb: truncated feature")
	}
	size := binary.LittleEndian.Uint32(sizeBuf[:])
	if size == 0 || size > 256<<20 {
		return nil, errors.New("fgb: invalid feature size")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r.in, buf); err != nil {
		return nil, errors.New("fgb: truncated feature")
	}
	return flattypes.GetRootAsFeature(buf, 0), nil
}

func treeSize(numItems uint64, nodeSize uint16) uint64 {
	if nodeSize < 2 || numItems == 0 {
		return 0
	}
	n := numItems
	numNodes := n
	for n != 1 {
		n = (n + uint64(nodeSize) - 1) / uint64(nodeSize)
		numNodes += n
	}
	return numNodes * 40
}

func Regions(data []byte, header *flattypes.Header) (featuresAt int, featuresLen int, err error) {
	headerLen := 4 + int(flatbuffers.GetUOffsetT(data[8:]))
	if headerLen <= 4 || headerLen > len(data) {
		return 0, 0, errors.New("fgb: invalid header length")
	}
	indexLen := int(treeSize(header.FeaturesCount(), header.IndexNodeSize()))
	featuresAt = 8 + headerLen + indexLen
	featuresLen = len(data) - featuresAt
	if featuresLen < 0 {
		return 0, 0, errors.New("fgb: truncated chunk")
	}
	return featuresAt, featuresLen, nil
}

const (
	NodeSize     uint16 = 16
	SegmentBytes int64  = 1 << 20
)

type Leaf struct {
	BBox   [4]float64
	Offset uint64
}

func Nodes(count uint64, nodeSize uint16) uint64 {
	nodes := count
	for n := count; n > 1; {
		n = (n + uint64(nodeSize) - 1) / uint64(nodeSize)
		nodes += n
	}
	return nodes
}

func Leaves(data []byte, count uint64, nodeSize uint16) []Leaf {
	if count == 0 || nodeSize < 2 {
		return nil
	}
	leaves := make([]Leaf, 0, count)
	for i := uint64(0); i < count; i++ {
		at := int((Nodes(count, nodeSize) - count + i) * 40)
		if at+40 > len(data) {
			break
		}
		leaves = append(leaves, Leaf{
			BBox: [4]float64{
				math.Float64frombits(binary.LittleEndian.Uint64(data[at:])),
				math.Float64frombits(binary.LittleEndian.Uint64(data[at+8:])),
				math.Float64frombits(binary.LittleEndian.Uint64(data[at+16:])),
				math.Float64frombits(binary.LittleEndian.Uint64(data[at+24:])),
			},
			Offset: binary.LittleEndian.Uint64(data[at+32:]),
		})
	}
	return leaves
}

func LeavesOf(data []byte, header *flattypes.Header) []Leaf {
	featuresAt, _, err := Regions(data, header)
	if err != nil {
		return nil
	}
	start := featuresAt - int(Nodes(header.FeaturesCount(), header.IndexNodeSize()))*40
	if start < 8 || featuresAt > len(data) {
		return nil
	}
	return Leaves(data[start:featuresAt], header.FeaturesCount(), header.IndexNodeSize())
}

func HeaderBlobOf(data []byte, header *flattypes.Header) []byte {
	featuresAt, _, err := Regions(data, header)
	if err != nil {
		return nil
	}
	end := featuresAt - int(Nodes(header.FeaturesCount(), header.IndexNodeSize()))*40
	if end <= 8 || end > len(data) {
		return nil
	}
	return data[8:end]
}

type item struct {
	offset uint64
	bbox   [4]float64
}

func (i item) NodeItem() index.NodeItem {
	return index.NewNodeItemWithCoordinates(i.offset, i.bbox[0], i.bbox[1], i.bbox[2], i.bbox[3])
}

func Index(chunks [][]Leaf, lengths []int64, nodeSize uint16) ([]byte, [4]float64, bool) {
	total := 0
	for _, leaves := range chunks {
		total += len(leaves)
	}
	if total == 0 {
		return nil, [4]float64{}, false
	}
	items := make([]index.Item, 0, total)
	var base int64
	for i, leaves := range chunks {
		for _, leaf := range leaves {
			items = append(items, item{offset: uint64(base) + leaf.Offset, bbox: leaf.BBox})
		}
		if i < len(lengths) {
			base += lengths[i]
		}
	}
	extent := index.CalcExtentForItems(items)
	var buf bytes.Buffer
	if _, err := index.NewPackedRTreeWithItems(items, extent, nodeSize).Write(&buf); err != nil {
		return nil, [4]float64{}, false
	}
	slice := extent.ToSlice()
	return buf.Bytes(), [4]float64{slice[0], slice[1], slice[2], slice[3]}, true
}

func Header(name string, blob []byte, count uint64, envelope [4]float64) ([]byte, bool) {
	src := flattypes.GetSizePrefixedRootAsHeader(blob, 0)
	if src == nil {
		return nil, false
	}
	builder := flatbuffers.NewBuilder(0)
	header := writer.NewHeader(builder).
		SetName(name).
		SetEnvelope([]float64{envelope[0], envelope[1], envelope[2], envelope[3]}).
		SetGeometryType(src.GeometryType()).
		SetHasZ(src.HasZ()).
		SetHasM(src.HasM()).
		SetFeaturesCount(count).
		SetIndexNodeSize(NodeSize)
	columns := make([]*writer.Column, src.ColumnsLength())
	for i := range columns {
		var column flattypes.Column
		src.Columns(&column, i)
		columns[i] = writer.NewColumn(builder).SetName(string(column.Name())).SetType(column.Type())
	}
	header.SetColumns(columns)
	var crs flattypes.Crs
	if src.Crs(&crs) != nil {
		header.SetCrs(writer.NewCrs(builder).
			SetOrg(string(crs.Org())).
			SetCode(crs.Code()).
			SetName(string(crs.Name())).
			SetCodeString(string(crs.CodeString())))
	}
	offset := header.Build()
	builder.FinishSizePrefixed(offset)
	return builder.FinishedBytes(), true
}
