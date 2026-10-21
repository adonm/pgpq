package vparquet

import (
	"slices"

	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
)

func cloneRowGroups(groups thrift.Slice[format.RowGroup]) thrift.Slice[format.RowGroup] {
	cloned := make(thrift.Slice[format.RowGroup], len(groups))
	for i := range groups {
		cloned[i] = cloneRowGroup(groups[i])
	}
	return cloned
}

func cloneRowGroup(group format.RowGroup) format.RowGroup {
	cloned := group
	cloned.Columns = make(thrift.Slice[format.ColumnChunk], len(group.Columns))
	for i := range group.Columns {
		cloned.Columns[i] = cloneColumnChunk(group.Columns[i])
	}
	cloned.SortingColumns = slices.Clone(group.SortingColumns)
	return cloned
}

func cloneColumnChunk(chunk format.ColumnChunk) format.ColumnChunk {
	cloned := chunk
	cloned.MetaData = cloneColumnMetaData(chunk.MetaData)
	cloned.EncryptedColumnMetadata = slices.Clone(chunk.EncryptedColumnMetadata)
	cloned.CryptoMetadata = format.ColumnCryptoMetaData{}
	return cloned
}

func cloneColumnMetaData(metadata format.ColumnMetaData) format.ColumnMetaData {
	cloned := metadata
	cloned.Encoding = slices.Clone(metadata.Encoding)
	cloned.PathInSchema = slices.Clone(metadata.PathInSchema)
	cloned.KeyValueMetadata = slices.Clone(metadata.KeyValueMetadata)
	cloned.EncodingStats = slices.Clone(metadata.EncodingStats)
	cloned.Statistics.Max = slices.Clone(metadata.Statistics.Max)
	cloned.Statistics.Min = slices.Clone(metadata.Statistics.Min)
	cloned.Statistics.MaxValue = slices.Clone(metadata.Statistics.MaxValue)
	cloned.Statistics.MinValue = slices.Clone(metadata.Statistics.MinValue)
	cloned.GeospatialStatistics.GeoSpatialTypes = slices.Clone(metadata.GeospatialStatistics.GeoSpatialTypes)
	cloned.SizeStatistics.RepetitionLevelHistogram = slices.Clone(metadata.SizeStatistics.RepetitionLevelHistogram)
	cloned.SizeStatistics.DefinitionLevelHistogram = slices.Clone(metadata.SizeStatistics.DefinitionLevelHistogram)
	return cloned
}

func rebaseRowGroup(group *format.RowGroup, delta int64) {
	if group.FileOffset != 0 {
		group.FileOffset += delta
	}
	for i := range group.Columns {
		rebaseColumnChunk(&group.Columns[i], delta)
	}
	dropPageIndexes(group)
}

func dropPageIndexes(group *format.RowGroup) {
	for i := range group.Columns {
		group.Columns[i].ColumnIndexOffset = 0
		group.Columns[i].ColumnIndexLength = 0
		group.Columns[i].OffsetIndexOffset = 0
		group.Columns[i].OffsetIndexLength = 0
	}
}

func rebaseColumnChunk(chunk *format.ColumnChunk, delta int64) {
	if chunk.FileOffset != 0 {
		chunk.FileOffset += delta
	}
	if chunk.ColumnIndexOffset != 0 {
		chunk.ColumnIndexOffset += delta
	}
	if chunk.OffsetIndexOffset != 0 {
		chunk.OffsetIndexOffset += delta
	}
	metadata := &chunk.MetaData
	if metadata.DataPageOffset != 0 {
		metadata.DataPageOffset += delta
	}
	if metadata.IndexPageOffset != 0 {
		metadata.IndexPageOffset += delta
	}
	if metadata.DictionaryPageOffset != 0 {
		metadata.DictionaryPageOffset += delta
	}
	if metadata.BloomFilterOffset != 0 {
		metadata.BloomFilterOffset += delta
	}
}

func mergeKeyValues(values thrift.Slice[format.KeyValue], geo string, hasGeo bool) thrift.Slice[format.KeyValue] {
	merged := make(thrift.Slice[format.KeyValue], 0, len(values)+1)
	replaced := false
	for i := range values {
		if values[i].Key == "geo" {
			if hasGeo && !replaced {
				merged = append(merged, format.KeyValue{Key: "geo", Value: geo})
				replaced = true
			}
			continue
		}
		merged = append(merged, values[i])
	}
	if hasGeo && !replaced {
		merged = append(merged, format.KeyValue{Key: "geo", Value: geo})
	}
	return merged
}
