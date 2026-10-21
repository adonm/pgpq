package vparquet

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/wkb"

	"github.com/adonm/pgpq/internal/geo"
)

func pointWKB(t *testing.T, x, y float64) []byte {
	t.Helper()
	raw, err := wkb.Marshal(geom.NewPointFlat(geom.XY, []float64{x, y}), wkb.NDR)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func pointFeature(t *testing.T, id string, key uint64, x, y float64, attributes map[string]any) geo.Feature {
	t.Helper()
	props := map[string]any{"id": id}
	for name, value := range attributes {
		props[name] = value
	}
	return geo.Feature{ID: id, Key: key, Geometry: pointWKB(t, x, y), Props: props}
}

func encodeShard(t *testing.T, columns []geo.Column, features []geo.Feature) []byte {
	t.Helper()
	for i := range features {
		bounds, ok := geo.Bounds(features[i].Geometry)
		if !ok {
			t.Fatalf("feature %d has no bounds", i)
		}
		features[i].Bounds = bounds
	}
	data, err := geo.EncodeShard(geo.Dataset{Columns: columns, Geometry: "geometry", IDColumn: "id"}, features)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type trackingReaderAt struct {
	reader io.ReaderAt
	reads  []trackedRead
}

type trackedRead struct {
	offset int64
	length int64
}

func (r *trackingReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	r.reads = append(r.reads, trackedRead{offset: offset, length: int64(len(buffer))})
	return r.reader.ReadAt(buffer, offset)
}

func assertMetadataOnlyReads(t *testing.T, reader *trackingReaderAt, data []byte) {
	t.Helper()
	_, footerStart := shardFooter(t, data)
	for _, read := range reader.reads {
		if read.offset < int64(footerStart) && read.offset+read.length > int64(len(magic)) {
			t.Fatalf("build read shard data at [%d,%d), footer starts at %d", read.offset, read.offset+read.length, footerStart)
		}
	}
}

func shardFooter(t *testing.T, data []byte) (format.FileMetaData, int) {
	t.Helper()
	if len(data) < 12 {
		t.Fatalf("shard is %d bytes", len(data))
	}
	length := int(binary.LittleEndian.Uint32(data[len(data)-8 : len(data)-4]))
	start := len(data) - 8 - length
	var meta format.FileMetaData
	if err := thrift.Unmarshal(compactProtocol, data[start:len(data)-8], &meta); err != nil {
		t.Fatal(err)
	}
	return meta, start
}

func rewriteShardFooter(t *testing.T, data []byte, mutate func(*format.FileMetaData)) []byte {
	t.Helper()
	meta, start := shardFooter(t, data)
	mutate(&meta)
	payload, err := thrift.Marshal(compactProtocol, &meta)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := append([]byte{}, data[:start]...)
	rewritten = append(rewritten, payload...)
	rewritten = binary.LittleEndian.AppendUint32(rewritten, uint32(len(payload)))
	rewritten = append(rewritten, magic...)
	return rewritten
}

func withGeometryTypes(t *testing.T, data []byte, types []string) []byte {
	t.Helper()
	return rewriteShardFooter(t, data, func(meta *format.FileMetaData) {
		for i := range meta.KeyValueMetadata {
			if meta.KeyValueMetadata[i].Key != "geo" {
				continue
			}
			var document map[string]any
			if err := json.Unmarshal([]byte(meta.KeyValueMetadata[i].Value), &document); err != nil {
				t.Fatal(err)
			}
			columns, _ := document["columns"].(map[string]any)
			for name, raw := range columns {
				column, _ := raw.(map[string]any)
				column["geometry_types"] = types
				columns[name] = column
			}
			encoded, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			meta.KeyValueMetadata[i].Value = string(encoded)
		}
	})
}

func assemble(t *testing.T, layout Layout, shards [][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	buffer.WriteString(magic)
	for _, segment := range layout.Segments {
		source := shards[segment.Source]
		buffer.Write(source[segment.SourceOffset : segment.SourceOffset+segment.Length])
	}
	if int64(buffer.Len()) != layout.DataLength {
		t.Fatalf("data length %d, layout says %d", buffer.Len(), layout.DataLength)
	}
	buffer.Write(layout.Footer)
	if int64(buffer.Len()) != layout.TotalLength {
		t.Fatalf("total length %d, layout says %d", buffer.Len(), layout.TotalLength)
	}
	return buffer.Bytes()
}

type virtualReader struct {
	shards []Shard
	layout Layout
}

func (v virtualReader) Size() int64 { return v.layout.TotalLength }

func (v virtualReader) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative read offset %d", offset)
	}
	if offset >= v.layout.TotalLength {
		return 0, io.EOF
	}
	written := 0
	for written < len(buffer) && offset+int64(written) < v.layout.TotalLength {
		position := offset + int64(written)
		count := 0
		switch {
		case position < int64(len(magic)):
			count = copy(buffer[written:], magic[position:])
		case position < v.layout.DataLength:
			segment, ok := v.segmentAt(position)
			if !ok {
				return written, fmt.Errorf("no segment covers virtual offset %d", position)
			}
			amount := min(int64(len(buffer)-written), segment.Offset+segment.Length-position)
			source := v.shards[segment.Source]
			sourceOffset := segment.SourceOffset + position - segment.Offset
			if err := readAt(source.Reader, buffer[written:written+int(amount)], sourceOffset); err != nil {
				return written, err
			}
			count = int(amount)
		default:
			footerOffset := position - v.layout.DataLength
			count = copy(buffer[written:], v.layout.Footer[footerOffset:])
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
		written += count
	}
	if written < len(buffer) {
		return written, io.EOF
	}
	return written, nil
}

func (v virtualReader) segmentAt(offset int64) (Segment, bool) {
	for _, segment := range v.layout.Segments {
		if offset >= segment.Offset && offset < segment.Offset+segment.Length {
			return segment, true
		}
	}
	return Segment{}, false
}

func compareFeatures(t *testing.T, got, want []geo.Feature) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d features, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("feature %d id = %q, want %q", i, got[i].ID, want[i].ID)
		}
		if !bytes.Equal(got[i].Geometry, want[i].Geometry) {
			t.Fatalf("feature %d geometry changed", i)
		}
		if !reflect.DeepEqual(got[i].Props, want[i].Props) {
			t.Fatalf("feature %d props = %#v, want %#v", i, got[i].Props, want[i].Props)
		}
	}
}

func decodeGeoMetadata(t *testing.T, meta format.FileMetaData) map[string]any {
	t.Helper()
	value, ok := findKeyValue(meta.KeyValueMetadata, "geo")
	if !ok {
		t.Fatal("merged metadata has no geo key")
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

type decodedRow struct {
	id       string
	geometry []byte
}

func rowColumnIndexes(t *testing.T, fields []parquet.Field) (int, int) {
	t.Helper()
	idColumn := -1
	geometryColumn := -1
	for i := range fields {
		switch fields[i].Name() {
		case "id":
			idColumn = i
		case "geometry":
			geometryColumn = i
		}
	}
	if idColumn < 0 || geometryColumn < 0 {
		t.Fatalf("schema lacks id or geometry: %v", fields)
	}
	return idColumn, geometryColumn
}

func decodeRow(row parquet.Row, idColumn, geometryColumn int) decodedRow {
	var decoded decodedRow
	for _, value := range row {
		switch int(value.Column()) {
		case idColumn:
			if !value.IsNull() {
				decoded.id = string(value.ByteArray())
			}
		case geometryColumn:
			if !value.IsNull() {
				decoded.geometry = append([]byte(nil), value.ByteArray()...)
			}
		}
	}
	return decoded
}

func readRows(t *testing.T, reader *parquet.Reader) []decodedRow {
	t.Helper()
	idColumn, geometryColumn := rowColumnIndexes(t, reader.Schema().Fields())
	var rows []decodedRow
	buffer := make([]parquet.Row, 128)
	for {
		n, err := reader.ReadRows(buffer)
		for i := 0; i < n; i++ {
			rows = append(rows, decodeRow(buffer[i], idColumn, geometryColumn))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	return rows
}

func readCurrentRow(t *testing.T, reader *parquet.Reader) decodedRow {
	t.Helper()
	idColumn, geometryColumn := rowColumnIndexes(t, reader.Schema().Fields())
	buffer := make([]parquet.Row, 1)
	n, err := reader.ReadRows(buffer)
	if n != 1 {
		t.Fatalf("read %d rows: %v", n, err)
	}
	return decodeRow(buffer[0], idColumn, geometryColumn)
}

func TestBuildVirtualGeoParquet(t *testing.T) {
	columns := []geo.Column{{Name: "id", Type: geo.String}, {Name: "n", Type: geo.Int}}
	featuresA := []geo.Feature{
		pointFeature(t, "a0", 1, 1, 2, map[string]any{"n": int64(10)}),
		pointFeature(t, "a1", 2, 2, 3, nil),
	}
	featuresB := make([]geo.Feature, 2049)
	for i := range featuresB {
		attributes := map[string]any(nil)
		if i%3 == 0 {
			attributes = map[string]any{"n": int64(i)}
		}
		featuresB[i] = pointFeature(t, fmt.Sprintf("b%04d", i), uint64(100+i), 10+float64(i)/1000, 20, attributes)
	}
	featuresC := []geo.Feature{pointFeature(t, "c0", 5000, -5, -6, map[string]any{"n": int64(-1)})}
	shards := [][]byte{
		encodeShard(t, columns, featuresA),
		encodeShard(t, columns, featuresB),
		encodeShard(t, columns, featuresC),
	}
	for i := range shards {
		shards[i] = withGeometryTypes(t, shards[i], []string{"Point"})
	}
	readers := make([]*trackingReaderAt, len(shards))
	inputs := make([]Shard, len(shards))
	for i := range shards {
		readers[i] = &trackingReaderAt{reader: bytes.NewReader(shards[i])}
		inputs[i] = Shard{Reader: readers[i], Size: int64(len(shards[i]))}
	}
	layout, err := Build(inputs)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := int64(len(featuresA) + len(featuresB) + len(featuresC))
	if layout.Rows != wantRows {
		t.Fatalf("layout rows = %d, want %d", layout.Rows, wantRows)
	}
	if len(layout.Segments) != len(shards) {
		t.Fatalf("segments = %d, want %d", len(layout.Segments), len(shards))
	}
	offset := int64(len(magic))
	for i, segment := range layout.Segments {
		_, footerStart := shardFooter(t, shards[i])
		if segment.Offset != offset {
			t.Fatalf("segment %d offset = %d, want %d", i, segment.Offset, offset)
		}
		if segment.Source != i {
			t.Fatalf("segment %d source = %d", i, segment.Source)
		}
		if segment.SourceOffset != int64(len(magic)) {
			t.Fatalf("segment %d source offset = %d", i, segment.SourceOffset)
		}
		if segment.Length != int64(footerStart-len(magic)) {
			t.Fatalf("segment %d length = %d, want %d", i, segment.Length, footerStart-len(magic))
		}
		offset += segment.Length
		assertMetadataOnlyReads(t, readers[i], shards[i])
	}
	if layout.DataLength != offset {
		t.Fatalf("data length = %d, want %d", layout.DataLength, offset)
	}
	if layout.TotalLength != layout.DataLength+int64(len(layout.Footer)) {
		t.Fatalf("total length = %d", layout.TotalLength)
	}
	if len(layout.Footer) < 8 || string(layout.Footer[len(layout.Footer)-4:]) != magic {
		t.Fatal("footer does not end with PAR1")
	}

	merged, _ := shardFooter(t, layout.Footer)
	if merged.NumRows != wantRows {
		t.Fatalf("merged metadata rows = %d", merged.NumRows)
	}
	if len(merged.RowGroups) != 4 {
		t.Fatalf("merged row groups = %d, want 4", len(merged.RowGroups))
	}
	leaves := leafElements(merged.Schema)
	geometryLeaf := -1
	for i := range leaves {
		if leaves[i].Name == "geometry" {
			geometryLeaf = i
		}
	}
	if geometryLeaf < 0 {
		t.Fatal("merged schema has no geometry leaf")
	}
	for groupIndex := range merged.RowGroups {
		bbox := merged.RowGroups[groupIndex].Columns[geometryLeaf].MetaData.GeospatialStatistics.BBox
		if bbox.XMin == 0 && bbox.XMax == 0 && bbox.YMin == 0 && bbox.YMax == 0 {
			t.Fatalf("row group %d lost its geospatial statistics", groupIndex)
		}
	}
	groupIndex := 0
	for shardIndex := range shards {
		source, _ := shardFooter(t, shards[shardIndex])
		segment := layout.Segments[shardIndex]
		for range source.RowGroups {
			group := merged.RowGroups[groupIndex]
			if group.Ordinal != int16(groupIndex) {
				t.Fatalf("row group %d ordinal = %d", groupIndex, group.Ordinal)
			}
			for columnIndex := range group.Columns {
				chunk := group.Columns[columnIndex]
				if chunk.ColumnIndexOffset != 0 || chunk.ColumnIndexLength != 0 || chunk.OffsetIndexOffset != 0 || chunk.OffsetIndexLength != 0 {
					t.Fatalf("shard %d row group %d column %d kept a page index", shardIndex, groupIndex, columnIndex)
				}
				offsets := []int64{
					group.FileOffset,
					chunk.FileOffset,
					chunk.ColumnIndexOffset,
					chunk.OffsetIndexOffset,
					chunk.MetaData.DataPageOffset,
					chunk.MetaData.DictionaryPageOffset,
					chunk.MetaData.IndexPageOffset,
					chunk.MetaData.BloomFilterOffset,
				}
				for _, value := range offsets {
					if value == 0 {
						continue
					}
					if value < segment.Offset || value >= segment.Offset+segment.Length {
						t.Fatalf("shard %d row group %d column %d offset %d outside segment [%d,%d)", shardIndex, groupIndex, columnIndex, value, segment.Offset, segment.Offset+segment.Length)
					}
				}
			}
			groupIndex++
		}
	}

	document := decodeGeoMetadata(t, merged)
	if version, _ := document["version"].(string); version != "2.0.0" {
		t.Fatalf("geo version = %q", version)
	}
	if primary, _ := document["primary_column"].(string); primary != "geometry" {
		t.Fatalf("primary column = %q", primary)
	}
	columnsDocument, _ := document["columns"].(map[string]any)
	geometryDocument, _ := columnsDocument["geometry"].(map[string]any)
	if encoding, _ := geometryDocument["encoding"].(string); encoding != "WKB" {
		t.Fatalf("geometry encoding = %q", encoding)
	}
	bbox, ok := geometryDocument["bbox"].([]any)
	if !ok || len(bbox) != 4 {
		t.Fatalf("bbox = %#v", geometryDocument["bbox"])
	}
	maxX := 10 + float64(len(featuresB)-1)/1000
	wantBox := []float64{-5, -6, maxX, 20}
	for i := range wantBox {
		if bbox[i] != wantBox[i] {
			t.Fatalf("bbox = %v, want %v", bbox, wantBox)
		}
	}
	types, _ := geometryDocument["geometry_types"].([]any)
	if len(types) != 1 || types[0] != "Point" {
		t.Fatalf("geometry_types = %v", geometryDocument["geometry_types"])
	}

	assembled := assemble(t, layout, shards)
	file, err := parquet.OpenFile(bytes.NewReader(assembled), int64(len(assembled)))
	if err != nil {
		t.Fatal(err)
	}
	if file.NumRows() != wantRows {
		t.Fatalf("parquet rows = %d, want %d", file.NumRows(), wantRows)
	}
	if len(file.RowGroups()) != 4 {
		t.Fatalf("parquet row groups = %d", len(file.RowGroups()))
	}
	sourceFile, err := parquet.OpenFile(bytes.NewReader(shards[0]), int64(len(shards[0])))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(file.Metadata().Schema, sourceFile.Metadata().Schema) {
		t.Fatal("merged schema differs from the shard schema")
	}

	opened, err := geo.ReadDataset(bytes.NewReader(assembled), geo.Options{IDColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]geo.Feature{}, featuresA...), featuresB...), featuresC...)
	compareFeatures(t, opened.Features, want)

	virtual := virtualReader{shards: inputs, layout: layout}
	regions := []struct {
		offset int64
		length int64
	}{
		{0, 64},
		{layout.Segments[0].Offset + layout.Segments[0].Length - 16, 32},
		{layout.Segments[1].Offset - 8, 24},
		{layout.Segments[2].Offset, 40},
		{layout.DataLength - 24, 48},
		{layout.DataLength, 16},
		{layout.TotalLength - 16, 16},
	}
	for _, region := range regions {
		if region.offset < 0 || region.offset+region.length > layout.TotalLength {
			continue
		}
		read := make([]byte, region.length)
		if err := readAt(virtual, read, region.offset); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read, assembled[region.offset:region.offset+region.length]) {
			t.Fatalf("virtual read [%d,%d) differs", region.offset, region.offset+region.length)
		}
	}

	virtualFile, err := parquet.OpenFile(virtual, layout.TotalLength)
	if err != nil {
		t.Fatal(err)
	}
	if virtualFile.NumRows() != wantRows {
		t.Fatalf("virtual rows = %d, want %d", virtualFile.NumRows(), wantRows)
	}
	reader := parquet.NewReader(virtualFile)
	virtualRows := readRows(t, reader)
	reader.Close()
	if len(virtualRows) != len(want) {
		t.Fatalf("virtual reader returned %d rows, want %d", len(virtualRows), len(want))
	}
	for i := range want {
		if virtualRows[i].id != want[i].ID {
			t.Fatalf("virtual row %d id = %q, want %q", i, virtualRows[i].id, want[i].ID)
		}
		if !bytes.Equal(virtualRows[i].geometry, want[i].Geometry) {
			t.Fatalf("virtual row %d geometry changed", i)
		}
	}
	seekReader := parquet.NewReader(virtualFile)
	seeks := []int64{0, 1, 2, 2 + int64(len(featuresB)) - 1, wantRows - 1}
	for _, rowIndex := range seeks {
		if err := seekReader.SeekToRow(rowIndex); err != nil {
			t.Fatalf("seek %d: %v", rowIndex, err)
		}
		row := readCurrentRow(t, seekReader)
		if row.id != want[rowIndex].ID || !bytes.Equal(row.geometry, want[rowIndex].Geometry) {
			t.Fatalf("seek %d read id %q", rowIndex, row.id)
		}
	}
	seekReader.Close()
}

func TestBuildSingleShard(t *testing.T) {
	features := []geo.Feature{
		pointFeature(t, "a", 1, 7, 8, map[string]any{"n": int64(3)}),
		pointFeature(t, "b", 2, 9, 10, nil),
	}
	data := encodeShard(t, []geo.Column{{Name: "id", Type: geo.String}, {Name: "n", Type: geo.Int}}, features)
	layout, err := Build([]Shard{{Reader: bytes.NewReader(data), Size: int64(len(data))}})
	if err != nil {
		t.Fatal(err)
	}
	if layout.Rows != 2 || len(layout.Segments) != 1 {
		t.Fatalf("layout = %+v", layout)
	}
	assembled := assemble(t, layout, [][]byte{data})
	dataset, err := geo.ReadDataset(bytes.NewReader(assembled), geo.Options{IDColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	compareFeatures(t, dataset.Features, features)
}

func TestBuildRejectsNoShards(t *testing.T) {
	if _, err := Build(nil); err == nil {
		t.Fatal("Build accepted no shards")
	}
}

func TestBuildRejectsEmptyShard(t *testing.T) {
	schema := parquet.NewSchema("geoparquet", parquet.Group{
		"geometry": parquet.Optional(parquet.Geometry("OGC:CRS84")),
	})
	var buffer bytes.Buffer
	writer := parquet.NewWriter(&buffer, &parquet.WriterConfig{
		Schema: schema,
		KeyValueMetadata: map[string]string{
			"geo": `{"version":"2.0.0","primary_column":"geometry","columns":{"geometry":{"encoding":"WKB","geometry_types":[],"crs":null}}}`,
		},
	})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	if _, err := Build([]Shard{{Reader: bytes.NewReader(data), Size: int64(len(data))}}); err == nil {
		t.Fatal("Build accepted an empty shard")
	}
}

func TestBuildRejectsNonNativeGeometry(t *testing.T) {
	schema := parquet.NewSchema("geoparquet", parquet.Group{
		"geometry": parquet.Optional(parquet.Leaf(parquet.ByteArrayType)),
	})
	var buffer bytes.Buffer
	writer := parquet.NewWriter(&buffer, &parquet.WriterConfig{Schema: schema})
	row := parquet.Row{parquet.ByteArrayValue(pointWKB(t, 1, 2)).Level(0, 1, 0)}
	if _, err := writer.WriteRows([]parquet.Row{row}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	if _, err := Build([]Shard{{Reader: bytes.NewReader(data), Size: int64(len(data))}}); err == nil {
		t.Fatal("Build accepted a non-native geometry column")
	}
}

func TestBuildRejectsSchemaMismatch(t *testing.T) {
	columnsA := []geo.Column{{Name: "id", Type: geo.String}}
	columnsB := []geo.Column{{Name: "id", Type: geo.String}, {Name: "n", Type: geo.Int}}
	features := []geo.Feature{pointFeature(t, "a", 1, 1, 1, nil)}
	shards := [][]byte{
		encodeShard(t, columnsA, features),
		encodeShard(t, columnsB, features),
	}
	inputs := make([]Shard, len(shards))
	for i := range shards {
		inputs[i] = Shard{Reader: bytes.NewReader(shards[i]), Size: int64(len(shards[i]))}
	}
	if _, err := Build(inputs); err == nil {
		t.Fatal("Build accepted differing schemas")
	}
}

func TestBuildRejectsGeoParquet11(t *testing.T) {
	data := encodeShard(t, []geo.Column{{Name: "id", Type: geo.String}}, []geo.Feature{pointFeature(t, "a", 1, 1, 1, nil)})
	rewritten := rewriteShardFooter(t, data, func(meta *format.FileMetaData) {
		for i := range meta.KeyValueMetadata {
			if meta.KeyValueMetadata[i].Key == "geo" {
				meta.KeyValueMetadata[i].Value = strings.Replace(meta.KeyValueMetadata[i].Value, `"2.0.0"`, `"1.1.0"`, 1)
			}
		}
	})
	if _, err := Build([]Shard{{Reader: bytes.NewReader(rewritten), Size: int64(len(rewritten))}}); err == nil {
		t.Fatal("Build accepted GeoParquet 1.1 metadata")
	}
}

func TestBuildRejectsExternalColumn(t *testing.T) {
	data := encodeShard(t, []geo.Column{{Name: "id", Type: geo.String}}, []geo.Feature{pointFeature(t, "a", 1, 1, 1, nil)})
	rewritten := rewriteShardFooter(t, data, func(meta *format.FileMetaData) {
		meta.RowGroups[0].Columns[0].FilePath = "external.parquet"
	})
	if _, err := Build([]Shard{{Reader: bytes.NewReader(rewritten), Size: int64(len(rewritten))}}); err == nil {
		t.Fatal("Build accepted an external column file path")
	}
}

func TestBuildRejectsEncryptedFooter(t *testing.T) {
	data := encodeShard(t, []geo.Column{{Name: "id", Type: geo.String}}, []geo.Feature{pointFeature(t, "a", 1, 1, 1, nil)})
	rewritten := rewriteShardFooter(t, data, func(meta *format.FileMetaData) {
		meta.EncryptionAlgorithm = format.EncryptionAlgorithm{Value: &format.AesGcmV1{}}
	})
	if _, err := Build([]Shard{{Reader: bytes.NewReader(rewritten), Size: int64(len(rewritten))}}); err == nil {
		t.Fatal("Build accepted an encrypted footer")
	}
}
