package vparquet

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
)

var compactProtocol = &thrift.CompactProtocol{}

type shardFile struct {
	meta       format.FileMetaData
	dataLength int64
	geometry   map[string]int
	geo        *geoDocument
}

func openShard(shard Shard, index int) (shardFile, error) {
	if shard.Reader == nil {
		return shardFile{}, invalid("shard %d has no reader", index)
	}
	if shard.Size < int64(len(magic))*2+8 {
		return shardFile{}, invalid("shard %d is too small to be parquet", index)
	}
	var header [4]byte
	if err := readAt(shard.Reader, header[:], 0); err != nil {
		return shardFile{}, invalid("shard %d header: %v", index, err)
	}
	switch string(header[:]) {
	case magic:
	case encryptedMagic:
		return shardFile{}, invalid("shard %d has an encrypted footer", index)
	default:
		return shardFile{}, invalid("shard %d is not a parquet file", index)
	}
	var trailer [8]byte
	if err := readAt(shard.Reader, trailer[:], shard.Size-8); err != nil {
		return shardFile{}, invalid("shard %d trailer: %v", index, err)
	}
	switch string(trailer[4:]) {
	case magic:
	case encryptedMagic:
		return shardFile{}, invalid("shard %d has an encrypted footer", index)
	default:
		return shardFile{}, invalid("shard %d has an invalid footer magic", index)
	}
	footerSize := int64(binary.LittleEndian.Uint32(trailer[:4]))
	footerStart := shard.Size - 8 - footerSize
	if footerSize == 0 || footerStart <= int64(len(magic)) {
		return shardFile{}, invalid("shard %d has an invalid footer length", index)
	}
	footer := make([]byte, footerSize)
	if err := readAt(shard.Reader, footer, footerStart); err != nil {
		return shardFile{}, invalid("shard %d footer: %v", index, err)
	}
	var meta format.FileMetaData
	if err := thrift.Unmarshal(compactProtocol, footer, &meta); err != nil {
		return shardFile{}, invalid("shard %d footer metadata: %v", index, err)
	}
	geometry, err := validateMetadata(&meta, footerStart, index)
	if err != nil {
		return shardFile{}, err
	}
	opened := shardFile{
		meta:       meta,
		dataLength: footerStart - int64(len(magic)),
		geometry:   geometry,
	}
	if value, ok := findKeyValue(meta.KeyValueMetadata, "geo"); ok {
		geo, err := parseGeo(value)
		if err != nil {
			return shardFile{}, invalid("shard %d geo metadata: %v", index, err)
		}
		if err := validateGeoGeometry(geo, geometry); err != nil {
			return shardFile{}, invalid("shard %d geo metadata: %v", index, err)
		}
		opened.geo = geo
	}
	return opened, nil
}

func readAt(reader io.ReaderAt, buffer []byte, offset int64) error {
	n, err := reader.ReadAt(buffer, offset)
	if n == len(buffer) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func validateMetadata(meta *format.FileMetaData, footerStart int64, index int) (map[string]int, error) {
	if meta.NumRows <= 0 {
		return nil, invalid("shard %d is empty", index)
	}
	if meta.EncryptionAlgorithm.Value != nil || len(meta.FooterSigningKeyMetadata) > 0 {
		return nil, invalid("shard %d uses footer encryption", index)
	}
	leaves := leafElements(meta.Schema)
	if len(leaves) == 0 {
		return nil, invalid("shard %d has no schema leaves", index)
	}
	geometry := make(map[string]int)
	for i := range leaves {
		if !isGeometryElement(leaves[i]) {
			continue
		}
		name := leaves[i].Name
		if _, ok := geometry[name]; ok {
			return nil, invalid("shard %d has duplicate geometry column %q", index, name)
		}
		geometry[name] = i
	}
	if len(geometry) == 0 {
		return nil, invalid("shard %d has no native parquet geometry column", index)
	}
	var rows int64
	for groupIndex := range meta.RowGroups {
		group := &meta.RowGroups[groupIndex]
		if group.NumRows < 0 {
			return nil, invalid("shard %d row group %d has a negative row count", index, groupIndex)
		}
		rows += group.NumRows
		if len(group.Columns) != len(leaves) {
			return nil, invalid("shard %d row group %d has %d columns, want %d", index, groupIndex, len(group.Columns), len(leaves))
		}
		if group.FileOffset != 0 && !inDataSection(group.FileOffset, footerStart) {
			return nil, invalid("shard %d row group %d file offset is outside the data section", index, groupIndex)
		}
		for columnIndex := range group.Columns {
			if err := validateColumn(&group.Columns[columnIndex], leaves[columnIndex], footerStart, index, groupIndex, columnIndex); err != nil {
				return nil, err
			}
		}
	}
	if rows != meta.NumRows {
		return nil, invalid("shard %d row groups hold %d rows, metadata says %d", index, rows, meta.NumRows)
	}
	return geometry, nil
}

func validateColumn(chunk *format.ColumnChunk, leaf format.SchemaElement, footerStart int64, shardIndex, groupIndex, columnIndex int) error {
	where := fmt.Sprintf("shard %d row group %d column %d", shardIndex, groupIndex, columnIndex)
	if chunk.FilePath != "" {
		return invalid("%s uses an external file path", where)
	}
	if chunk.CryptoMetadata.Value != nil || len(chunk.EncryptedColumnMetadata) > 0 {
		return invalid("%s is encrypted", where)
	}
	metadata := &chunk.MetaData
	if len(metadata.PathInSchema) == 0 {
		return invalid("%s has no column metadata", where)
	}
	if !leaf.Type.Valid || metadata.Type != leaf.Type.V {
		return invalid("%s physical type does not match the schema", where)
	}
	checks := []struct {
		label  string
		offset int64
		length int64
	}{
		{where + " file", chunk.FileOffset, 0},
		{where + " column index", chunk.ColumnIndexOffset, int64(chunk.ColumnIndexLength)},
		{where + " offset index", chunk.OffsetIndexOffset, int64(chunk.OffsetIndexLength)},
		{where + " dictionary page", metadata.DictionaryPageOffset, 0},
		{where + " index page", metadata.IndexPageOffset, 0},
		{where + " bloom filter", metadata.BloomFilterOffset, int64(metadata.BloomFilterLength)},
	}
	for _, check := range checks {
		if err := checkOffset(check.label, check.offset, check.length, footerStart); err != nil {
			return err
		}
	}
	if metadata.DataPageOffset == 0 {
		if metadata.TotalCompressedSize != 0 || metadata.NumValues != 0 {
			return invalid("%s has no data page offset", where)
		}
		return nil
	}
	return checkOffset(where+" data page", metadata.DataPageOffset, 0, footerStart)
}

func checkOffset(label string, offset, length, footerStart int64) error {
	if length < 0 {
		return invalid("%s has a negative length", label)
	}
	if offset == 0 {
		if length != 0 {
			return invalid("%s has a length without an offset", label)
		}
		return nil
	}
	if !inDataSection(offset, footerStart) {
		return invalid("%s points outside the shard data section", label)
	}
	if length > footerStart-offset {
		return invalid("%s extends beyond the shard data section", label)
	}
	return nil
}

func inDataSection(offset, footerStart int64) bool {
	return offset >= int64(len(magic)) && offset < footerStart
}

func leafElements(schema thrift.Slice[format.SchemaElement]) []format.SchemaElement {
	leaves := make([]format.SchemaElement, 0, len(schema))
	for i := range schema {
		if schema[i].Type.Valid {
			leaves = append(leaves, schema[i])
		}
	}
	return leaves
}

func isGeometryElement(element format.SchemaElement) bool {
	if element.LogicalType.Value == nil {
		return false
	}
	switch element.LogicalType.Value.(type) {
	case *format.GeometryType, *format.GeographyType:
		return true
	}
	return false
}

func findKeyValue(values []format.KeyValue, key string) (string, bool) {
	for i := range values {
		if values[i].Key == key {
			return values[i].Value, true
		}
	}
	return "", false
}
