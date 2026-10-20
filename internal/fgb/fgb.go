package fgb

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"

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
	f, _, err := r.NextRaw()
	return f, err
}

func (r *Reader) NextRaw() (*flattypes.Feature, []byte, error) {
	var sizeBuf [4]byte
	if _, err := io.ReadFull(r.in, sizeBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, io.EOF
		}
		return nil, nil, errors.New("fgb: truncated feature")
	}
	size := binary.LittleEndian.Uint32(sizeBuf[:])
	if size == 0 || size > 256<<20 {
		return nil, nil, errors.New("fgb: invalid feature size")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r.in, buf); err != nil {
		return nil, nil, errors.New("fgb: truncated feature")
	}
	raw := make([]byte, 4+len(buf))
	copy(raw, sizeBuf[:])
	copy(raw[4:], buf)
	return flattypes.GetRootAsFeature(buf, 0), raw, nil
}

func treeSize(numItems uint64, nodeSize uint16) uint64 {
	if nodeSize < 2 || numItems == 0 {
		return 0
	}
	nodes := numItems
	for n := numItems; n > 1; {
		n = (n + uint64(nodeSize) - 1) / uint64(nodeSize)
		nodes += n
	}
	return nodes * 40
}
