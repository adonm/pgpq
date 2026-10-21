package vparquet

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"

	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
)

const (
	magic          = "PAR1"
	encryptedMagic = "PARE"
)

type Shard struct {
	Reader io.ReaderAt
	Size   int64
}

type Segment struct {
	Offset       int64
	Length       int64
	Source       int
	SourceOffset int64
}

type Layout struct {
	Footer      []byte
	DataLength  int64
	TotalLength int64
	Rows        int64
	Segments    []Segment
}

func Build(shards []Shard) (Layout, error) {
	if len(shards) == 0 {
		return Layout{}, invalid("no shards")
	}
	opened := make([]shardFile, len(shards))
	for i := range shards {
		shard, err := openShard(shards[i], i)
		if err != nil {
			return Layout{}, err
		}
		opened[i] = shard
	}
	for i := 1; i < len(opened); i++ {
		if !reflect.DeepEqual(opened[0].meta.Schema, opened[i].meta.Schema) {
			return Layout{}, invalid("shard %d schema differs from shard 0", i)
		}
	}
	geoValue, hasGeo, err := mergeGeoMetadata(opened)
	if err != nil {
		return Layout{}, err
	}
	merged := format.FileMetaData{
		Version:          opened[0].meta.Version,
		Schema:           slices.Clone(opened[0].meta.Schema),
		RowGroups:        make(thrift.Slice[format.RowGroup], 0, rowGroupCount(opened)),
		KeyValueMetadata: mergeKeyValues(opened[0].meta.KeyValueMetadata, geoValue, hasGeo),
		CreatedBy:        opened[0].meta.CreatedBy,
		ColumnOrders:     slices.Clone(opened[0].meta.ColumnOrders),
	}
	segments := make([]Segment, 0, len(opened))
	dataLength := int64(len(magic))
	for i := range opened {
		base := dataLength
		delta := base - int64(len(magic))
		groups := cloneRowGroups(opened[i].meta.RowGroups)
		for j := range groups {
			rebaseRowGroup(&groups[j], delta)
			groups[j].Ordinal = int16(len(merged.RowGroups) + j)
		}
		merged.RowGroups = append(merged.RowGroups, groups...)
		merged.NumRows += opened[i].meta.NumRows
		segments = append(segments, Segment{
			Offset:       base,
			Length:       opened[i].dataLength,
			Source:       i,
			SourceOffset: int64(len(magic)),
		})
		dataLength += opened[i].dataLength
	}
	footer, err := encodeFooter(&merged)
	if err != nil {
		return Layout{}, err
	}
	return Layout{
		Footer:      footer,
		DataLength:  dataLength,
		TotalLength: dataLength + int64(len(footer)),
		Rows:        merged.NumRows,
		Segments:    segments,
	}, nil
}

func encodeFooter(meta *format.FileMetaData) ([]byte, error) {
	payload, err := thrift.Marshal(compactProtocol, meta)
	if err != nil {
		return nil, invalid("encode footer: %v", err)
	}
	if uint64(len(payload)) > uint64(math.MaxUint32)-8 {
		return nil, invalid("footer length %d exceeds the parquet limit", len(payload))
	}
	footer := make([]byte, 0, len(payload)+8)
	footer = append(footer, payload...)
	footer = binary.LittleEndian.AppendUint32(footer, uint32(len(payload)))
	footer = append(footer, magic...)
	return footer, nil
}

func rowGroupCount(shards []shardFile) int {
	total := 0
	for i := range shards {
		total += len(shards[i].meta.RowGroups)
	}
	return total
}

func invalid(message string, args ...any) error {
	return fmt.Errorf("vparquet: "+message, args...)
}
