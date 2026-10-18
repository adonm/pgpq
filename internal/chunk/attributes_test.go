package chunk

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
)

func TestAttrRoundTrip(t *testing.T) {
	cols := []Column{
		{Name: "b", Type: flattypes.ColumnTypeBool},
		{Name: "i", Type: flattypes.ColumnTypeInt},
		{Name: "f", Type: flattypes.ColumnTypeDouble},
		{Name: "s", Type: flattypes.ColumnTypeString},
		{Name: "j", Type: flattypes.ColumnTypeJson},
		{Name: "tags", Type: flattypes.ColumnTypeJson},
	}
	rows := []map[string]json.RawMessage{
		{
			"b": json.RawMessage("true"), "i": json.RawMessage("1"), "f": json.RawMessage("1.5"),
			"s": json.RawMessage(`"a"`), "j": json.RawMessage(`{"k":1}`), "tags": json.RawMessage(`["x","y"]`),
		},
		{"b": json.RawMessage("false"), "tags": json.RawMessage(`["x"]`)},
		{
			"b": json.RawMessage("true"), "i": json.RawMessage("42"), "f": json.RawMessage("2.25"),
			"s": json.RawMessage(`"b"`), "j": json.RawMessage(`{"k":2}`), "tags": json.RawMessage(`[]`),
		},
	}
	blob, err := EncodeAttrs(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := ReadAttrHeader(blob)
	if !ok {
		t.Fatal("header unreadable")
	}
	if file.Rows != 3 || len(file.Columns) != len(cols) {
		t.Fatalf("header rows=%d columns=%d", file.Rows, len(file.Columns))
	}
	if file.Columns[0].Kind != AttrBool || !file.Columns[5].Kind.IsList() {
		t.Fatalf("kinds %v %v", file.Columns[0].Kind, file.Columns[5].Kind)
	}
	if file.Columns[1].Min == nil || *file.Columns[1].Min != 1 || *file.Columns[1].Max != 42 {
		t.Fatalf("int min/max %v %v", file.Columns[1].Min, file.Columns[1].Max)
	}
	decoded, err := DecodeAttrs(blob)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		for _, col := range cols {
			raw := row[col.Name]
			got := decoded[i][col.Name]
			if isAttrNull(raw) && got == nil {
				continue
			}
			want := attrNorm(t, raw)
			if mine := attrMarshal(t, got); mine != want {
				t.Fatalf("row %d %s: got %s want %s", i, col.Name, mine, want)
			}
		}
	}
}

func TestAttrColumnIsIndependent(t *testing.T) {
	cols := []Column{
		{Name: "keep", Type: flattypes.ColumnTypeString},
		{Name: "damage", Type: flattypes.ColumnTypeString},
	}
	rows := []map[string]json.RawMessage{
		{"keep": json.RawMessage(`"first"`), "damage": json.RawMessage(`"gone"`)},
		{"keep": json.RawMessage(`"second"`), "damage": json.RawMessage(`"lost"`)},
	}
	blob, err := EncodeAttrs(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := ReadAttrHeader(blob)
	damage, _ := file.Column("damage")
	for i := damage.Offset; i < damage.Offset+damage.Length; i++ {
		blob[i] = 0
	}
	values, err := DecodeAttrColumn(blob, "keep")
	if err != nil {
		t.Fatalf("decoding keep after damaging damage: %v", err)
	}
	if len(values) != 2 || values[0] != "first" || values[1] != "second" {
		t.Fatalf("keep = %v", values)
	}
}

func TestAttrCorpusRoundTrip(t *testing.T) {
	data, err := os.ReadFile("../../testdata/corpus/mixed.fgb")
	if err != nil {
		t.Fatal(err)
	}
	c, err := FromRaw(data)
	if err != nil {
		t.Fatal(err)
	}
	cols := c.Columns()
	features, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]map[string]json.RawMessage, len(features))
	for i, f := range features {
		rows[i] = DecodePropertyText(f.PropertiesBytes(), cols)
	}
	for i := range cols {
		if cols[i].Name == "c_json" || cols[i].Name == "c_tags" {
			cols[i].Type = flattypes.ColumnTypeJson
			for _, row := range rows {
				row[cols[i].Name] = innerAttrJSON(row[cols[i].Name])
			}
		}
	}
	blob, err := EncodeAttrs(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAttrs(blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(rows) {
		t.Fatalf("rows %d", len(decoded))
	}
	needles := 0
	for i := range rows {
		if !isAttrNull(rows[i]["c_json"]) {
			object, ok := decoded[i]["c_json"].(map[string]any)
			if !ok {
				t.Fatalf("row %d c_json %T", i, decoded[i]["c_json"])
			}
			if _, ok := object["kind"]; !ok {
				t.Fatalf("row %d c_json lacks kind", i)
			}
		}
		tags, _ := decoded[i]["c_tags"].([]any)
		for _, tag := range tags {
			if tag == "needle_tag" {
				needles++
			}
		}
	}
	if needles != 10 {
		t.Fatalf("needle_tag memberships %d", needles)
	}
	if column, ok := attrHeaderFor(t, blob).Column("c_tags"); !ok || !column.Kind.IsList() {
		t.Fatalf("c_tags is not a list column")
	}
}

func attrHeaderFor(t *testing.T, blob []byte) AttrFile {
	t.Helper()
	file, ok := ReadAttrHeader(blob)
	if !ok {
		t.Fatal("header unreadable")
	}
	return file
}

func innerAttrJSON(raw json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) == nil && len(text) > 0 && (text[0] == '[' || text[0] == '{') {
		return json.RawMessage(text)
	}
	return raw
}

func isAttrNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func attrNorm(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("bad raw %s: %v", raw, err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func attrMarshal(t *testing.T, value any) string {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func BenchmarkAttrProjection(b *testing.B) {
	data, err := os.ReadFile("../../testdata/corpus/mixed.fgb")
	if err != nil {
		b.Fatal(err)
	}
	c, err := FromRaw(data)
	if err != nil {
		b.Fatal(err)
	}
	cols := c.Columns()
	features, err := c.SearchAll()
	if err != nil {
		b.Fatal(err)
	}
	rows := make([]map[string]json.RawMessage, len(features))
	for i, f := range features {
		rows[i] = DecodePropertyText(f.PropertiesBytes(), cols)
	}
	blob, err := EncodeAttrs(cols, rows)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("project-one-column", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := DecodeAttrColumn(blob, "c_string"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("project-typed", func(b *testing.B) {
		reader, ok := OpenAttrs(blob)
		if !ok {
			b.Fatal("header")
		}
		for i := 0; i < b.N; i++ {
			if _, err := reader.Values("c_string"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode-all-columns", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := DecodeAttrs(blob); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("maintainer-path", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			parsed, err := FromRaw(data)
			if err != nil {
				b.Fatal(err)
			}
			all, err := parsed.SearchAll()
			if err != nil {
				b.Fatal(err)
			}
			for _, f := range all {
				DecodeProperties(f.PropertiesBytes(), cols)
			}
		}
	})
	b.Run("encode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := EncodeAttrs(cols, rows); err != nil {
				b.Fatal(err)
			}
		}
	})
}
