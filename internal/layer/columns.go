package layer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
	"github.com/parquet-go/parquet-go/format"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
)

const (
	parquetMagic        = "PAR1"
	parquetRowGroupRows = 2048
)

type columnKind uint8

const (
	columnKindBool columnKind = iota
	columnKindInt
	columnKindDouble
	columnKindString
	columnKindJSON
)

type columnSummary struct {
	Name  string
	Nulls int
	Min   *float64
	Max   *float64
}

func encodeParquetColumns(cols []chunk.Column, rows []map[string]json.RawMessage) ([]byte, []columnSummary, error) {
	kinds := make([]columnKind, len(cols))
	nodes := make(parquet.Group, len(cols))
	for i, col := range cols {
		kinds[i] = parquetColumnKind(col.Type)
		nodes[col.Name] = parquet.Optional(parquetNodeOf(kinds[i]))
	}
	schema := parquet.NewSchema("columns", nodes)
	fields := schema.Fields()
	summaries := make([]columnSummary, len(cols))
	summaryOf := make(map[string]*columnSummary, len(cols))
	kindOf := make(map[string]columnKind, len(cols))
	for i, col := range cols {
		summaries[i].Name = col.Name
		summaryOf[col.Name] = &summaries[i]
		kindOf[col.Name] = kinds[i]
	}
	out := make([]parquet.Row, len(rows))
	for r := range rows {
		row := make(parquet.Row, 0, len(fields))
		for c, field := range fields {
			name := field.Name()
			value, ok := parquetValue(kindOf[name], rows[r][name])
			if !ok {
				summaryOf[name].Nulls++
				row = append(row, parquet.NullValue().Level(0, 0, c))
				continue
			}
			if kind := kindOf[name]; kind == columnKindInt || kind == columnKindDouble {
				summaryOf[name].keep(value)
			}
			row = append(row, value.Level(0, 1, c))
		}
		out[r] = row
	}
	var buf bytes.Buffer
	writer := parquet.NewWriter(&buf, &parquet.WriterConfig{
		Schema:             schema,
		MaxRowsPerRowGroup: parquetRowGroupRows,
		Compression:        &zstd.Codec{Level: zstd.SpeedBetterCompression},
	})
	if _, err := writer.WriteRows(out); err != nil {
		return nil, nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), summaries, nil
}

func (s *columnSummary) keep(value parquet.Value) {
	var f float64
	if value.Kind() == parquet.Double {
		f = value.Double()
	} else {
		f = float64(value.Int64())
	}
	if s.Min == nil || f < *s.Min {
		v := f
		s.Min = &v
	}
	if s.Max == nil || f > *s.Max {
		v := f
		s.Max = &v
	}
}

func parquetColumnKind(t flattypes.ColumnType) columnKind {
	switch t {
	case flattypes.ColumnTypeBool:
		return columnKindBool
	case flattypes.ColumnTypeByte, flattypes.ColumnTypeUByte,
		flattypes.ColumnTypeShort, flattypes.ColumnTypeUShort,
		flattypes.ColumnTypeInt, flattypes.ColumnTypeUInt,
		flattypes.ColumnTypeLong, flattypes.ColumnTypeULong:
		return columnKindInt
	case flattypes.ColumnTypeFloat, flattypes.ColumnTypeDouble:
		return columnKindDouble
	case flattypes.ColumnTypeJson:
		return columnKindJSON
	}
	return columnKindString
}

func parquetNodeOf(kind columnKind) parquet.Node {
	switch kind {
	case columnKindBool:
		return parquet.Leaf(parquet.BooleanType)
	case columnKindInt:
		return parquet.Leaf(parquet.Int64Type)
	case columnKindDouble:
		return parquet.Leaf(parquet.DoubleType)
	case columnKindJSON:
		return parquet.JSON()
	}
	return parquet.String()
}

func parquetValue(kind columnKind, raw json.RawMessage) (parquet.Value, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return parquet.Value{}, false
	}
	switch kind {
	case columnKindBool:
		switch string(raw) {
		case "true":
			return parquet.BooleanValue(true), true
		case "false":
			return parquet.BooleanValue(false), true
		}
	case columnKindInt:
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(string(raw), 64)
			if ferr != nil {
				return parquet.Value{}, false
			}
			n = int64(f)
		}
		return parquet.Int64Value(n), true
	case columnKindDouble:
		f, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return parquet.Value{}, false
		}
		return parquet.DoubleValue(f), true
	case columnKindJSON:
		return parquet.ByteArrayValue(append([]byte(nil), raw...)), true
	default:
		return parquet.ByteArrayValue(unquoteColumnText(raw)), true
	}
	return parquet.Value{}, false
}

func unquoteColumnText(raw []byte) []byte {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return append([]byte(nil), raw...)
	}
	body := raw[1 : len(raw)-1]
	for _, b := range body {
		if b == '\\' {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				return []byte(text)
			}
			return append([]byte(nil), raw...)
		}
	}
	return append([]byte(nil), body...)
}

func parquetMask(data []byte, filter cql.Expr, rows int) ([]bool, error) {
	return parquetMaskAt(bytes.NewReader(data), int64(len(data)), filter, rows)
}

func parquetMaskAt(r io.ReaderAt, size int64, filter cql.Expr, rows int) ([]bool, error) {
	keep := make([]bool, rows)
	err := parquetMatchesAt(r, size, filter, rows, 0, nil, func(row int) bool {
		keep[row] = true
		return true
	})
	return keep, err
}

func parquetZone(fields []parquet.Field, group format.RowGroup) map[string]zoneColumn {
	zone := make(map[string]zoneColumn, len(fields))
	for i, field := range fields {
		if i >= len(group.Columns) {
			break
		}
		stats := group.Columns[i].MetaData.Statistics
		z := zoneColumn{Nulls: int(stats.NullCount)}
		known := stats.NullCount > 0
		switch field.Type().Kind() {
		case parquet.Int64:
			min, minOK := parquetInt64(stats.MinValue)
			max, maxOK := parquetInt64(stats.MaxValue)
			if minOK && maxOK {
				lo, hi := float64(min), float64(max)
				z.Min, z.Max = &lo, &hi
				known = true
			}
		case parquet.Double:
			min, minOK := parquetDouble(stats.MinValue)
			max, maxOK := parquetDouble(stats.MaxValue)
			if minOK && maxOK {
				lo, hi := min, max
				z.Min, z.Max = &lo, &hi
				known = true
			}
		}
		if known {
			zone[field.Name()] = z
		}
	}
	return zone
}

func parquetInt64(b []byte) (int64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(b)), true
}

func parquetDouble(b []byte) (float64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), true
}

func columnTypeOf(t parquet.Type) flattypes.ColumnType {
	if logical := t.LogicalType(); logical != nil {
		switch logical.Value.(type) {
		case *format.JsonType:
			return flattypes.ColumnTypeJson
		case *format.StringType:
			return flattypes.ColumnTypeString
		}
	}
	switch t.Kind() {
	case parquet.Boolean:
		return flattypes.ColumnTypeBool
	case parquet.Int64:
		return flattypes.ColumnTypeLong
	case parquet.Double:
		return flattypes.ColumnTypeDouble
	}
	return flattypes.ColumnTypeString
}

func parquetMatchGroupRows(group parquet.RowGroup, schema *parquet.Schema, names []string, types map[string]flattypes.ColumnType, filter cql.Expr, base int, emit func(int) bool) (bool, error) {
	count := int(group.NumRows())
	if len(names) == 0 {
		if cql.Match(filter, nil, types) {
			for i := 0; i < count; i++ {
				if !emit(base + i) {
					return false, nil
				}
			}
		}
		return true, nil
	}
	reader := parquet.NewRowGroupReader(group, &parquet.ReaderConfig{Schema: schema})
	defer reader.Close()
	buffer := make([]parquet.Row, min(count, 256))
	read := 0
	for read < count {
		n, err := reader.ReadRows(buffer)
		for i := 0; i < n; i++ {
			props := make(map[string]any, len(buffer[i]))
			for _, value := range buffer[i] {
				if value.IsNull() {
					continue
				}
				column := value.Column()
				if column < 0 || column >= len(names) {
					return false, errors.New("layer: column file row is malformed")
				}
				props[names[column]] = parquetGoValue(types[names[column]], value)
			}
			if cql.Match(filter, props, types) {
				if !emit(base + read + i) {
					return false, nil
				}
			}
		}
		read += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return false, err
		}
		if n == 0 {
			break
		}
	}
	if read != count {
		return false, fmt.Errorf("layer: column file row group has %d rows, want %d", read, count)
	}
	return true, nil
}

func parquetMatchesAt(r io.ReaderAt, size int64, filter cql.Expr, rows int, start int, within []int, emit func(int) bool) error {
	file, err := parquet.OpenFile(r, size, parquet.SkipPageIndex(true))
	if err != nil {
		return errors.New("layer: column file unreadable")
	}
	if file.NumRows() != int64(rows) {
		return fmt.Errorf("layer: column file has %d rows, chunk has %d", file.NumRows(), rows)
	}
	if start < 0 {
		start = 0
	}
	if start >= rows {
		return nil
	}
	fields := file.Schema().Fields()
	types := make(map[string]flattypes.ColumnType, len(fields))
	wanted := make(map[string]bool)
	for _, name := range cql.Properties(filter) {
		wanted[name] = true
	}
	projected := make(parquet.Group)
	var names []string
	for _, field := range fields {
		types[field.Name()] = columnTypeOf(field.Type())
		if !wanted[field.Name()] {
			continue
		}
		node := parquet.Node(parquet.Leaf(field.Type()))
		if field.Optional() {
			node = parquet.Optional(node)
		} else {
			node = parquet.Required(node)
		}
		projected[field.Name()] = node
		names = append(names, field.Name())
	}
	var projectedSchema *parquet.Schema
	if len(names) > 0 {
		projectedSchema = parquet.NewSchema("columns", projected)
	}
	metadata := file.Metadata().RowGroups
	groups := file.RowGroups()
	offset := 0
	for i, group := range groups {
		count := int(group.NumRows())
		if offset+count > rows {
			return errors.New("layer: column file has more rows than chunk")
		}
		wanted := within == nil || (sort.SearchInts(within, offset) < len(within) && within[sort.SearchInts(within, offset)] < offset+count)
		if offset+count > start && wanted && mayMatch(filter, parquetZone(fields, metadata[i])) {
			stop := false
			cont, err := parquetMatchGroupRows(group, projectedSchema, names, types, filter, offset, func(row int) bool {
				if stop || row < start {
					return true
				}
				if !emit(row) {
					stop = true
					return false
				}
				return true
			})
			if err != nil {
				return err
			}
			if !cont {
				return nil
			}
		}
		offset += count
	}
	if offset != rows {
		return fmt.Errorf("layer: column file has %d rows, chunk has %d", offset, rows)
	}
	return nil
}

func parquetGoValue(column flattypes.ColumnType, value parquet.Value) any {
	switch value.Kind() {
	case parquet.Boolean:
		return value.Boolean()
	case parquet.Int64:
		return value.Int64()
	case parquet.Double:
		return value.Double()
	case parquet.ByteArray, parquet.FixedLenByteArray:
		raw := value.ByteArray()
		if column == flattypes.ColumnTypeJson {
			var out any
			if json.Unmarshal(raw, &out) == nil {
				return out
			}
		}
		return string(raw)
	}
	return nil
}
