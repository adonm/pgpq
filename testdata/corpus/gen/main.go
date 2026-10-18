package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"
)

const count = 30000

type column struct {
	name string
	typ  flattypes.ColumnType
}

var columns = []column{
	{"id", flattypes.ColumnTypeInt},
	{"c_bool", flattypes.ColumnTypeBool},
	{"c_byte", flattypes.ColumnTypeByte},
	{"c_ubyte", flattypes.ColumnTypeUByte},
	{"c_short", flattypes.ColumnTypeShort},
	{"c_ushort", flattypes.ColumnTypeUShort},
	{"c_uint", flattypes.ColumnTypeUInt},
	{"c_long", flattypes.ColumnTypeLong},
	{"c_ulong", flattypes.ColumnTypeULong},
	{"c_float", flattypes.ColumnTypeFloat},
	{"c_double", flattypes.ColumnTypeDouble},
	{"c_string", flattypes.ColumnTypeString},
	{"c_date", flattypes.ColumnTypeDateTime},
	{"c_json", flattypes.ColumnTypeString},
	{"c_tags", flattypes.ColumnTypeString},
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var kinds = []string{"alpha", "beta", "gamma", "delta"}

func missing(i int) bool { return i%1500 == 0 }

func feature(i int) ([]byte, float64, float64) {
	var props []byte
	add := func(index int, value []byte) {
		if value == nil {
			return
		}
		props = binary.LittleEndian.AppendUint16(props, uint16(index))
		props = append(props, value...)
	}
	add(0, le32(uint32(i)))
	add(1, flag(i%2 == 0))
	if !missing(i) {
		add(2, []byte{byte(int8(i%9 - 4))})
	}
	add(3, []byte{byte((i % 10) * 25)})
	add(4, le16(uint16(int16(i%2000-1000))))
	add(5, le16(uint16(30000+i%1000)))
	add(6, le32(uint32(i*3)))
	add(7, le64(uint64(int64(i)*1000003)))
	if !missing(i) {
		add(8, le64(uint64(10000000000+i%10000)))
	}
	if !missing(i) {
		add(9, le32(math.Float32bits(float32(i%100)/2)))
	}
	add(10, le64(math.Float64bits(float64(i%1000)/8+0.125)))
	if !missing(i) {
		add(11, str(stringValue(i)))
	}
	add(12, str(epoch.Add(time.Duration(i)*time.Hour).Format(time.RFC3339)))
	if !missing(i) {
		add(13, str(jsonValue(i)))
		add(14, str(tagsValue(i)))
	}
	return props, -10 + float64(i%100)*0.2, 40 + float64(i/100)*0.03
}

func tagsValue(i int) string {
	out := fmt.Sprintf(`[%q,%q`, "r"+strconv.Itoa(i%7), "u"+strconv.Itoa(i%2))
	if i%3000 == 7 {
		out += `,"needle_tag"`
	}
	return out + "]"
}

func stringValue(i int) string {
	if i%3000 == 7 {
		return "needle"
	}
	return fmt.Sprintf("cat_%02d", i%25)
}

func jsonValue(i int) string {
	tags := fmt.Sprintf("[%q,%q", "t"+strconv.Itoa(i%5), "u"+strconv.Itoa(i%2))
	if i%3000 == 7 {
		tags += `,"needle_tag"`
	}
	tags += "]"
	out := fmt.Sprintf(`{"kind":%q,"score":%d,"flag":%t,"tags":%s,"label":%q`,
		kinds[i%4], i%1000, i%2 == 0, tags, fmt.Sprintf("L%03d", i%50))
	if i%3 == 0 {
		out += fmt.Sprintf(`,"sparse":%q`, "s"+strconv.Itoa(i%7))
	}
	return out + "}"
}

func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
func str(s string) []byte  { return append(le32(uint32(len(s))), s...) }
func flag(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

type generator struct{ next int }

func (g *generator) Generate() *writer.Feature {
	if g.next >= count {
		return nil
	}
	props, x, y := feature(g.next)
	g.next++
	b := flatbuffers.NewBuilder(0)
	return writer.NewFeature(b).
		SetGeometry(writer.NewGeometry(b).SetType(flattypes.GeometryTypePoint).SetXY([]float64{x, y})).
		SetProperties(props)
}

func main() {
	out := "testdata/corpus/mixed.fgb"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	hb := flatbuffers.NewBuilder(0)
	header := writer.NewHeader(hb).
		SetName("corpus_mixed").
		SetGeometryType(flattypes.GeometryTypePoint).
		SetCrs(writer.NewCrs(hb).SetOrg("EPSG").SetCode(4326).SetCodeString("EPSG:4326").SetName("WGS 84"))
	wcols := make([]*writer.Column, len(columns))
	for i, col := range columns {
		wcols[i] = writer.NewColumn(hb).SetName(col.name).SetType(col.typ)
	}
	header.SetColumns(wcols)
	w := writer.NewWriter(header, true, &generator{}, nil, writer.WithMemory())
	var buf bytes.Buffer
	if _, err := w.Write(&buf); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
