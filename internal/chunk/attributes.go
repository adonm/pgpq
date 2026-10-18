package chunk

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

type AttrKind uint8

const (
	AttrBool AttrKind = iota
	AttrInt
	AttrFloat
	AttrText
	AttrJSON
	attrList AttrKind = 0x80
	attrDict AttrKind = 0x40
)

func (k AttrKind) List() AttrKind { return k | attrList }
func (k AttrKind) IsList() bool   { return k&attrList != 0 }
func (k AttrKind) Dict() bool     { return k&attrDict != 0 }
func (k AttrKind) Base() AttrKind { return k &^ (attrList | attrDict) }
func (k AttrKind) column() uint8  { return uint8(k) }

type AttrColumn struct {
	Name   string
	Kind   AttrKind
	Rows   int
	Nulls  int
	Min    *float64
	Max    *float64
	Offset int
	Length int
	Stored int
}

type AttrFile struct {
	Rows    int
	Columns []AttrColumn
}

func (f AttrFile) Column(name string) (AttrColumn, bool) {
	for _, column := range f.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return AttrColumn{}, false
}

func (f AttrFile) ColumnTypes() map[string]flattypes.ColumnType {
	types := make(map[string]flattypes.ColumnType, len(f.Columns))
	for _, column := range f.Columns {
		types[column.Name] = column.Kind.ColumnType()
	}
	return types
}

func (k AttrKind) ColumnType() flattypes.ColumnType {
	switch k.Base() {
	case AttrBool:
		return flattypes.ColumnTypeBool
	case AttrInt:
		return flattypes.ColumnTypeLong
	case AttrFloat:
		return flattypes.ColumnTypeDouble
	case AttrJSON:
		return flattypes.ColumnTypeJson
	}
	return flattypes.ColumnTypeString
}

type AttrValues struct {
	Kind    AttrKind
	Rows    int
	Present []bool
	Bools   []bool
	Ints    []int64
	Floats  []float64
	Texts   []string
	Raw     [][]byte
	Offsets []uint32
	Child   *AttrValues
}

func (v AttrValues) Has(i int) bool {
	return i >= 0 && i < len(v.Present) && v.Present[i]
}

func (v AttrValues) Value(i int) any {
	if !v.Has(i) {
		return nil
	}
	if v.Kind.IsList() {
		return v.List(i)
	}
	switch v.Kind.Base() {
	case AttrBool:
		return v.Bools[i]
	case AttrInt:
		return v.Ints[i]
	case AttrFloat:
		return v.Floats[i]
	case AttrText:
		return v.Texts[i]
	case AttrJSON:
		var out any
		if json.Unmarshal(v.Raw[i], &out) == nil {
			return out
		}
		return string(v.Raw[i])
	}
	return nil
}

func (v AttrValues) List(i int) []any {
	if !v.Has(i) || v.Child == nil {
		return nil
	}
	lo, hi := int(v.Offsets[i]), int(v.Offsets[i+1])
	out := make([]any, 0, hi-lo)
	for j := lo; j < hi; j++ {
		if value := v.Child.Value(j); value != nil {
			out = append(out, value)
		}
	}
	return out
}

type AttrReader struct {
	file AttrFile
	data []byte
}

func OpenAttrs(data []byte) (*AttrReader, bool) {
	file, ok := ReadAttrHeader(data)
	if !ok {
		return nil, false
	}
	return &AttrReader{file: file, data: data}, true
}

func (r *AttrReader) File() AttrFile {
	return r.file
}

func (r *AttrReader) Values(name string) (AttrValues, error) {
	column, ok := r.file.Column(name)
	if !ok {
		return AttrValues{}, fmt.Errorf("chunk: no attribute column %q", name)
	}
	if column.Offset < 0 || column.Offset+column.Length > len(r.data) {
		return AttrValues{}, errors.New("chunk: attribute column out of range")
	}
	section, err := Decompress(r.data[column.Offset : column.Offset+column.Length])
	if err != nil {
		return AttrValues{}, err
	}
	return decodeAttrValues(column.Kind, section, r.file.Rows)
}

const attrMagic = "PFA1"

func EncodeAttrs(cols []Column, rows []map[string]json.RawMessage) ([]byte, error) {
	file := AttrFile{Rows: len(rows)}
	sections := make([][]byte, len(cols))
	for i, col := range cols {
		kind := attrKind(col, rows)
		section, nulls, min, max, err := encodeAttrValues(kind, attrValues(rows, col.Name))
		if err != nil {
			return nil, err
		}
		file.Columns = append(file.Columns, AttrColumn{
			Name: col.Name, Kind: kind, Rows: len(rows), Nulls: nulls,
			Min: min, Max: max, Stored: len(section),
		})
		sections[i] = Compress(section)
	}
	offset := len(encodeAttrHeader(file))
	for i := range file.Columns {
		file.Columns[i].Offset = offset
		file.Columns[i].Length = len(sections[i])
		offset += len(sections[i])
	}
	out := encodeAttrHeader(file)
	for _, section := range sections {
		out = append(out, section...)
	}
	return out, nil
}

func ReadAttrHeader(data []byte) (AttrFile, bool) {
	var file AttrFile
	if len(data) < 10 || string(data[:4]) != attrMagic {
		return file, false
	}
	file.Rows = int(binary.LittleEndian.Uint32(data[4:8]))
	count := int(binary.LittleEndian.Uint16(data[8:10]))
	off := 10
	for i := 0; i < count; i++ {
		if off+2 > len(data) {
			return file, false
		}
		nameLen := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if off+nameLen+1+4+1 > len(data) {
			return file, false
		}
		column := AttrColumn{Name: string(data[off : off+nameLen]), Rows: file.Rows}
		off += nameLen
		column.Kind = AttrKind(data[off])
		off++
		column.Nulls = int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if data[off] == 1 {
			if off+1+8 > len(data) {
				return file, false
			}
			min := math.Float64frombits(binary.LittleEndian.Uint64(data[off+1:]))
			column.Min = &min
			off += 9
		} else {
			off++
		}
		if data[off] == 1 {
			if off+1+8 > len(data) {
				return file, false
			}
			max := math.Float64frombits(binary.LittleEndian.Uint64(data[off+1:]))
			column.Max = &max
			off += 9
		} else {
			off++
		}
		if off+12 > len(data) {
			return file, false
		}
		column.Offset = int(binary.LittleEndian.Uint32(data[off:]))
		column.Length = int(binary.LittleEndian.Uint32(data[off+4:]))
		column.Stored = int(binary.LittleEndian.Uint32(data[off+8:]))
		off += 12
		file.Columns = append(file.Columns, column)
	}
	return file, true
}

func DecodeAttrs(data []byte) ([]map[string]any, error) {
	file, ok := ReadAttrHeader(data)
	if !ok {
		return nil, errors.New("chunk: attribute blob unreadable")
	}
	out := make([]map[string]any, file.Rows)
	for _, column := range file.Columns {
		values, err := DecodeAttrColumn(data, column.Name)
		if err != nil {
			return nil, err
		}
		for i, value := range values {
			if value == nil {
				continue
			}
			if out[i] == nil {
				out[i] = map[string]any{}
			}
			out[i][column.Name] = value
		}
	}
	return out, nil
}

func DecodeAttrColumn(data []byte, name string) ([]any, error) {
	reader, ok := OpenAttrs(data)
	if !ok {
		return nil, errors.New("chunk: attribute blob unreadable")
	}
	values, err := reader.Values(name)
	if err != nil {
		return nil, err
	}
	out := make([]any, values.Rows)
	for i := range out {
		out[i] = values.Value(i)
	}
	return out, nil
}

func encodeAttrHeader(file AttrFile) []byte {
	var b bytes.Buffer
	b.WriteString(attrMagic)
	var tmp [8]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(file.Rows))
	b.Write(tmp[:4])
	binary.LittleEndian.PutUint16(tmp[:], uint16(len(file.Columns)))
	b.Write(tmp[:2])
	for _, column := range file.Columns {
		binary.LittleEndian.PutUint16(tmp[:], uint16(len(column.Name)))
		b.Write(tmp[:2])
		b.WriteString(column.Name)
		b.WriteByte(column.Kind.column())
		binary.LittleEndian.PutUint32(tmp[:], uint32(column.Nulls))
		b.Write(tmp[:4])
		b.WriteByte(flagByte(column.Min != nil))
		if column.Min != nil {
			binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(*column.Min))
			b.Write(tmp[:8])
		}
		b.WriteByte(flagByte(column.Max != nil))
		if column.Max != nil {
			binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(*column.Max))
			b.Write(tmp[:8])
		}
		binary.LittleEndian.PutUint32(tmp[:], uint32(column.Offset))
		b.Write(tmp[:4])
		binary.LittleEndian.PutUint32(tmp[:], uint32(column.Length))
		b.Write(tmp[:4])
		binary.LittleEndian.PutUint32(tmp[:], uint32(column.Stored))
		b.Write(tmp[:4])
	}
	return b.Bytes()
}

func attrKind(col Column, rows []map[string]json.RawMessage) AttrKind {
	switch col.Type {
	case flattypes.ColumnTypeBool:
		return AttrBool
	case flattypes.ColumnTypeByte, flattypes.ColumnTypeUByte,
		flattypes.ColumnTypeShort, flattypes.ColumnTypeUShort,
		flattypes.ColumnTypeInt, flattypes.ColumnTypeUInt,
		flattypes.ColumnTypeLong, flattypes.ColumnTypeULong:
		return AttrInt
	case flattypes.ColumnTypeFloat, flattypes.ColumnTypeDouble:
		return AttrFloat
	case flattypes.ColumnTypeString, flattypes.ColumnTypeDateTime, flattypes.ColumnTypeBinary:
		if kind := attrTextKind(rows, col.Name); kind != 0 {
			return kind
		}
		return AttrText
	case flattypes.ColumnTypeJson:
		list := false
		for _, row := range rows {
			value, ok := attrValue(row[col.Name])
			if !ok {
				continue
			}
			if len(value) == 0 || value[0] != '[' {
				return AttrJSON
			}
			list = true
		}
		if list {
			return AttrJSON.List()
		}
		return AttrJSON
	}
	return AttrText
}

const attrDictBudget = 128

func attrTextKind(rows []map[string]json.RawMessage, name string) AttrKind {
	if len(rows) < attrDictBudget {
		return 0
	}
	distinct := map[string]struct{}{}
	for _, row := range rows {
		value, ok := attrValue(row[name])
		if !ok {
			continue
		}
		distinct[string(unquote(value))] = struct{}{}
		if len(distinct) > attrDictBudget {
			return 0
		}
	}
	if len(distinct) == 0 {
		return 0
	}
	return AttrText | attrDict
}

func attrValues(rows []map[string]json.RawMessage, name string) []json.RawMessage {
	out := make([]json.RawMessage, len(rows))
	for i, row := range rows {
		out[i] = row[name]
	}
	return out
}

func attrValue(value json.RawMessage) (json.RawMessage, bool) {
	if len(value) == 0 || string(value) == "null" {
		return nil, false
	}
	return value, true
}

func encodeAttrValues(kind AttrKind, values []json.RawMessage) ([]byte, int, *float64, *float64, error) {
	rows := len(values)
	valid := make([]byte, (rows+7)/8)
	nulls := 0
	var min, max *float64
	var body bytes.Buffer
	set := func(i int) { valid[i/8] |= 1 << (uint(i) % 8) }
	keep := func(f float64) {
		if min == nil || f < *min {
			v := f
			min = &v
		}
		if max == nil || f > *max {
			v := f
			max = &v
		}
	}
	if kind.IsList() {
		offsets := make([]uint32, rows+1)
		var elements []json.RawMessage
		for i, value := range values {
			text, ok := attrValue(value)
			if !ok {
				nulls++
				offsets[i+1] = uint32(len(elements))
				continue
			}
			var members []json.RawMessage
			if err := json.Unmarshal(text, &members); err != nil {
				return nil, 0, nil, nil, fmt.Errorf("chunk: attribute %d is not a list: %w", i, err)
			}
			set(i)
			elements = append(elements, members...)
			offsets[i+1] = uint32(len(elements))
		}
		body.Write(attrU32s(offsets))
		child, _, _, _, err := encodeAttrValues(kind.Base(), elements)
		if err != nil {
			return nil, 0, nil, nil, err
		}
		body.Write(child)
	} else {
		switch kind.Base() {
		case AttrBool:
			buf := make([]byte, rows)
			for i, value := range values {
				text, ok := attrValue(value)
				if !ok {
					nulls++
					continue
				}
				switch string(text) {
				case "true":
					buf[i] = 1
					set(i)
				case "false":
					set(i)
				default:
					nulls++
				}
			}
			body.Write(buf)
		case AttrInt:
			buf := make([]byte, rows*8)
			for i, value := range values {
				text, ok := attrValue(value)
				if !ok {
					nulls++
					continue
				}
				n, err := strconv.ParseInt(string(text), 10, 64)
				if err != nil {
					f, ferr := strconv.ParseFloat(string(text), 64)
					if ferr != nil {
						nulls++
						continue
					}
					n = int64(f)
				}
				binary.LittleEndian.PutUint64(buf[i*8:], uint64(n))
				set(i)
				keep(float64(n))
			}
			body.Write(buf)
		case AttrFloat:
			buf := make([]byte, rows*8)
			for i, value := range values {
				text, ok := attrValue(value)
				if !ok {
					nulls++
					continue
				}
				f, err := strconv.ParseFloat(string(text), 64)
				if err != nil {
					nulls++
					continue
				}
				binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(f))
				set(i)
				keep(f)
			}
			body.Write(buf)
		case AttrText:
			texts := make([][]byte, rows)
			for i, value := range values {
				text, ok := attrValue(value)
				if !ok {
					nulls++
					continue
				}
				texts[i] = unquote(text)
				set(i)
			}
			if kind.Dict() {
				body.WriteByte(1)
				body.Write(encodeTextDict(texts))
			} else {
				body.WriteByte(0)
				body.Write(encodeByteOffsets(texts))
			}
		case AttrJSON:
			raw := make([][]byte, rows)
			for i, value := range values {
				text, ok := attrValue(value)
				if !ok {
					nulls++
					continue
				}
				raw[i] = text
				set(i)
			}
			body.WriteByte(0)
			body.Write(encodeByteOffsets(raw))
		}
	}
	out := make([]byte, 0, len(valid)+body.Len())
	out = append(out, valid...)
	out = append(out, body.Bytes()...)
	return out, nulls, min, max, nil
}

func encodeByteOffsets(values [][]byte) []byte {
	offsets := make([]uint32, len(values)+1)
	var blob []byte
	for i, value := range values {
		blob = append(blob, value...)
		offsets[i+1] = uint32(len(blob))
	}
	out := attrU32s(offsets)
	return append(out, blob...)
}

func encodeTextDict(texts [][]byte) []byte {
	index := map[string]uint32{}
	var order []string
	for _, text := range texts {
		if text == nil {
			continue
		}
		if _, ok := index[string(text)]; !ok {
			index[string(text)] = uint32(len(order))
			order = append(order, string(text))
		}
	}
	dictOffsets := make([]uint32, len(order)+1)
	var dictBlob []byte
	for i, text := range order {
		dictBlob = append(dictBlob, text...)
		dictOffsets[i+1] = uint32(len(dictBlob))
	}
	codes := make([]uint32, len(texts))
	for i, text := range texts {
		if text != nil {
			codes[i] = index[string(text)]
		}
	}
	var b bytes.Buffer
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(order)))
	b.Write(tmp[:])
	b.Write(attrU32s(dictOffsets))
	b.Write(dictBlob)
	b.Write(attrU32s(codes))
	return b.Bytes()
}

func decodeAttrValues(kind AttrKind, section []byte, rows int) (AttrValues, error) {
	values := AttrValues{Kind: kind, Rows: rows, Present: make([]bool, rows)}
	validLen := (rows + 7) / 8
	if len(section) < validLen {
		return values, errors.New("chunk: attribute section truncated")
	}
	valid, body := section[:validLen], section[validLen:]
	for i := 0; i < rows; i++ {
		values.Present[i] = valid[i/8]&(1<<(uint(i)%8)) != 0
	}
	if kind.IsList() {
		offsets, rest, ok := attrReadU32s(body, rows+1)
		if !ok {
			return values, errors.New("chunk: attribute list offsets truncated")
		}
		values.Offsets = offsets
		child, err := decodeAttrValues(kind.Base(), rest, int(offsets[rows]))
		if err != nil {
			return values, err
		}
		values.Child = &child
		return values, nil
	}
	switch kind.Base() {
	case AttrBool:
		if len(body) < rows {
			return values, errors.New("chunk: attribute bools truncated")
		}
		values.Bools = make([]bool, rows)
		for i := 0; i < rows; i++ {
			if values.Present[i] {
				values.Bools[i] = body[i] != 0
			}
		}
	case AttrInt:
		if len(body) < rows*8 {
			return values, errors.New("chunk: attribute ints truncated")
		}
		values.Ints = make([]int64, rows)
		for i := 0; i < rows; i++ {
			if values.Present[i] {
				values.Ints[i] = int64(binary.LittleEndian.Uint64(body[i*8:]))
			}
		}
	case AttrFloat:
		if len(body) < rows*8 {
			return values, errors.New("chunk: attribute floats truncated")
		}
		values.Floats = make([]float64, rows)
		for i := 0; i < rows; i++ {
			if values.Present[i] {
				values.Floats[i] = math.Float64frombits(binary.LittleEndian.Uint64(body[i*8:]))
			}
		}
	case AttrText:
		if len(body) < 1 {
			return values, errors.New("chunk: attribute text truncated")
		}
		mode := body[0]
		body = body[1:]
		if mode == 1 {
			return decodeTextDict(values, body)
		}
		raw, err := decodeByteOffsets(body, rows)
		if err != nil {
			return values, err
		}
		values.Texts = make([]string, rows)
		for i := 0; i < rows; i++ {
			if values.Present[i] {
				values.Texts[i] = string(raw[i])
			}
		}
	case AttrJSON:
		if len(body) < 1 {
			return values, errors.New("chunk: attribute json truncated")
		}
		body = body[1:]
		raw, err := decodeByteOffsets(body, rows)
		if err != nil {
			return values, err
		}
		values.Raw = raw
	}
	return values, nil
}

func decodeTextDict(values AttrValues, body []byte) (AttrValues, error) {
	if len(body) < 4 {
		return values, errors.New("chunk: attribute dictionary truncated")
	}
	count := int(binary.LittleEndian.Uint32(body))
	rest := body[4:]
	dictOffsets, rest, ok := attrReadU32s(rest, count+1)
	if !ok {
		return values, errors.New("chunk: attribute dictionary offsets truncated")
	}
	dictLen := int(dictOffsets[count])
	if len(rest) < dictLen+values.Rows*4 {
		return values, errors.New("chunk: attribute dictionary truncated")
	}
	dict := make([]string, count)
	for i := 0; i < count; i++ {
		dict[i] = string(rest[dictOffsets[i]:dictOffsets[i+1]])
	}
	codes := rest[dictLen:]
	values.Texts = make([]string, values.Rows)
	for i := 0; i < values.Rows; i++ {
		if values.Present[i] {
			values.Texts[i] = dict[binary.LittleEndian.Uint32(codes[i*4:])]
		}
	}
	return values, nil
}

func decodeByteOffsets(body []byte, rows int) ([][]byte, error) {
	offsets, blob, ok := attrReadU32s(body, rows+1)
	if !ok {
		return nil, errors.New("chunk: attribute offsets truncated")
	}
	out := make([][]byte, rows)
	for i := 0; i < rows; i++ {
		lo, hi := int(offsets[i]), int(offsets[i+1])
		if hi > len(blob) {
			return nil, errors.New("chunk: attribute blob truncated")
		}
		out[i] = blob[lo:hi]
	}
	return out, nil
}

func attrU32s(values []uint32) []byte {
	out := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(out[i*4:], value)
	}
	return out
}

func attrReadU32s(body []byte, n int) ([]uint32, []byte, bool) {
	if n < 0 || len(body) < n*4 {
		return nil, nil, false
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(body[i*4:])
	}
	return out, body[n*4:], true
}

func flagByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}
