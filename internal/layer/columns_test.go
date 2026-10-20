package layer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/parquet-go/parquet-go"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
)

func TestParquetColumnTypes(t *testing.T) {
	cols := []chunk.Column{
		{Name: "b", Type: flattypes.ColumnTypeBool},
		{Name: "i", Type: flattypes.ColumnTypeLong},
		{Name: "d", Type: flattypes.ColumnTypeDouble},
		{Name: "s", Type: flattypes.ColumnTypeString},
		{Name: "j", Type: flattypes.ColumnTypeJson},
		{Name: "n", Type: flattypes.ColumnTypeLong},
	}
	rows := []map[string]json.RawMessage{
		{"b": json.RawMessage("true"), "i": json.RawMessage("42"), "d": json.RawMessage("1.5"), "s": json.RawMessage(`"hé"`), "j": json.RawMessage(`{"k":1}`), "n": json.RawMessage("7")},
		{"b": json.RawMessage("false"), "i": json.RawMessage("-7"), "d": json.RawMessage("2.25"), "s": json.RawMessage(`"x"`), "j": json.RawMessage(`["a","b"]`)},
		{},
	}
	blob, summaries, err := encodeParquetColumns(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(blob, []byte(parquetMagic)) {
		t.Fatal("column file does not start with PAR1")
	}
	got := parquetRows(t, blob)
	if len(got) != len(rows) {
		t.Fatalf("%d rows, want %d", len(got), len(rows))
	}
	if got[0]["b"] != true || got[1]["b"] != false || got[2]["b"] != nil {
		t.Fatalf("bools = %#v", got)
	}
	if got[0]["i"] != int64(42) || got[1]["i"] != int64(-7) || got[2]["i"] != nil {
		t.Fatalf("ints = %#v", got)
	}
	if got[0]["d"] != 1.5 || got[1]["d"] != 2.25 || got[2]["d"] != nil {
		t.Fatalf("doubles = %#v", got)
	}
	if got[0]["s"] != "hé" || got[1]["s"] != "x" || got[2]["s"] != nil {
		t.Fatalf("strings = %#v", got)
	}
	if !reflect.DeepEqual(got[0]["j"], map[string]any{"k": 1.0}) {
		t.Fatalf("json object = %#v", got[0]["j"])
	}
	if !reflect.DeepEqual(got[1]["j"], []any{"a", "b"}) {
		t.Fatalf("json list = %#v", got[1]["j"])
	}
	if got[2]["j"] != nil || got[0]["n"] != int64(7) || got[1]["n"] != nil {
		t.Fatalf("nulls = %#v", got)
	}
	if summaries[1].Min == nil || *summaries[1].Min != -7 || *summaries[1].Max != 42 {
		t.Fatalf("int summary = %+v", summaries[1])
	}
	if summaries[2].Min == nil || *summaries[2].Min != 1.5 || *summaries[2].Max != 2.25 {
		t.Fatalf("double summary = %+v", summaries[2])
	}
	if summaries[5].Nulls != 2 {
		t.Fatalf("null summary = %+v", summaries[5])
	}
}

func TestParquetRowGroupsOf2048(t *testing.T) {
	cols := []chunk.Column{{Name: "id", Type: flattypes.ColumnTypeString}}
	rows := make([]map[string]json.RawMessage, 4097)
	for i := range rows {
		rows[i] = map[string]json.RawMessage{"id": json.RawMessage(strconv.Quote(fmt.Sprintf("f-%05d", i)))}
	}
	blob, _, err := encodeParquetColumns(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parquet.OpenFile(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	if file.NumRows() != 4097 {
		t.Fatalf("file has %d rows", file.NumRows())
	}
	var sizes []int64
	for _, group := range file.RowGroups() {
		sizes = append(sizes, group.NumRows())
	}
	if !slices.Equal(sizes, []int64{2048, 2048, 1}) {
		t.Fatalf("row groups = %v, want [2048 2048 1]", sizes)
	}
}

func TestParquetFilterPlans(t *testing.T) {
	cols := []chunk.Column{{Name: "n", Type: flattypes.ColumnTypeLong}}
	rows := make([]map[string]json.RawMessage, 4097)
	for i := range rows {
		rows[i] = map[string]json.RawMessage{"n": json.RawMessage(strconv.Itoa(i))}
	}
	blob, _, err := encodeParquetColumns(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]bool{
		"n >= 0":     true,
		"n < 0":      false,
		"n = 999999": false,
		"n IS NULL":  false,
	} {
		expr, err := cql.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parquetMask(blob, expr, len(rows))
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		for i, keep := range got {
			if keep != want {
				t.Fatalf("%s: row %d = %v, want %v", text, i, keep, want)
			}
		}
	}
}

func parquetRows(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	file, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	fields := file.Schema().Fields()
	names := make([]string, len(fields))
	types := make(map[string]flattypes.ColumnType, len(fields))
	for i, field := range fields {
		names[i] = field.Name()
		types[field.Name()] = columnTypeOf(field.Type())
	}
	var out []map[string]any
	for _, group := range file.RowGroups() {
		reader := parquet.NewRowGroupReader(group)
		rows := make([]parquet.Row, group.NumRows())
		read := 0
		for read < len(rows) {
			n, err := reader.ReadRows(rows[read:])
			read += n
			if err != nil {
				break
			}
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if read != len(rows) {
			t.Fatalf("read %d of %d rows", read, len(rows))
		}
		for _, row := range rows {
			props := map[string]any{}
			for _, value := range row {
				if !value.IsNull() {
					props[names[value.Column()]] = parquetGoValue(types[names[value.Column()]], value)
				}
			}
			out = append(out, props)
		}
	}
	return out
}

func countDifferent(a, b []bool) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

type countingReaderAt struct {
	r io.ReaderAt
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

func TestParquetMaskRangeReads(t *testing.T) {
	cols := []chunk.Column{{Name: "n", Type: flattypes.ColumnTypeLong}}
	rows := make([]map[string]json.RawMessage, 4097)
	for i := range rows {
		rows[i] = map[string]json.RawMessage{"n": json.RawMessage(strconv.Itoa(i))}
	}
	blob, _, err := encodeParquetColumns(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := cql.Parse("n = 999999")
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingReaderAt{r: bytes.NewReader(blob)}
	got, err := parquetMaskAt(counter, int64(len(blob)), expr, len(rows))
	if err != nil {
		t.Fatal(err)
	}
	for i, keep := range got {
		if keep {
			t.Fatalf("row %d matched an impossible filter", i)
		}
	}
	if counter.n >= int64(len(blob)) {
		t.Fatalf("range read fetched the whole file: %d of %d", counter.n, len(blob))
	}
}

func TestParquetMatchesStopsEarly(t *testing.T) {
	cols := []chunk.Column{{Name: "n", Type: flattypes.ColumnTypeLong}}
	rows := make([]map[string]json.RawMessage, 4097)
	for i := range rows {
		rows[i] = map[string]json.RawMessage{"n": json.RawMessage(strconv.Itoa(i))}
	}
	blob, _, err := encodeParquetColumns(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := cql.Parse("n >= 0")
	if err != nil {
		t.Fatal(err)
	}
	count, last := 0, -1
	err = parquetMatchesAt(bytes.NewReader(blob), int64(len(blob)), expr, len(rows), 0, nil, func(row int) bool {
		count++
		last = row
		return count < 10
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 10 || last != 9 {
		t.Fatalf("early stop visited %d rows, last %d", count, last)
	}
	count, last = 0, -1
	err = parquetMatchesAt(bytes.NewReader(blob), int64(len(blob)), expr, len(rows), 2048, nil, func(row int) bool {
		count++
		last = row
		return count < 3
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || last != 2050 {
		t.Fatalf("started read visited %d rows, last %d", count, last)
	}
}
