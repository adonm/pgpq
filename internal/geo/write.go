package geo

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"

	"github.com/adonm/pgpq/internal/cql"
	"github.com/adonm/pgpq/internal/spatial"
)

const rowGroupRows = 2048

type cellLayout struct {
	fields   []parquet.Field
	key      int
	geometry int
	types    []cql.Type
}

func EncodeCell(ds Dataset, features []Feature) ([]byte, error) {
	if len(features) == 0 {
		return nil, invalid("no features to encode")
	}
	nodes := make(parquet.Group, len(ds.Columns)+2)
	nodes["__pgpq_key"] = parquet.Optional(parquet.Leaf(parquet.Int64Type))
	nodes[ds.Geometry] = parquet.Optional(parquet.Geometry("OGC:CRS84"))
	for _, col := range ds.Columns {
		nodes[col.Name] = parquet.Optional(nodeOf(col.Type))
	}
	schema := parquet.NewSchema("geoparquet", nodes)
	layout := layoutOf(ds, schema)
	metadata, err := geoMetadata(ds, FeatureBounds(features))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer := parquet.NewWriter(&buf, &parquet.WriterConfig{
		Schema:             schema,
		MaxRowsPerRowGroup: rowGroupRows,
		Compression:        &zstd.Codec{Level: zstd.SpeedBetterCompression},
		KeyValueMetadata:   map[string]string{"geo": metadata},
	})
	rows := make([]parquet.Row, 0, rowGroupRows)
	for _, feature := range features {
		rows = append(rows, layout.row(feature))
		if len(rows) == rowGroupRows {
			if _, err := writer.WriteRows(rows); err != nil {
				return nil, err
			}
			rows = rows[:0]
		}
	}
	if len(rows) > 0 {
		if _, err := writer.WriteRows(rows); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func layoutOf(ds Dataset, schema *parquet.Schema) cellLayout {
	fields := schema.Fields()
	layout := cellLayout{fields: fields, key: -1, geometry: -1, types: make([]cql.Type, len(fields))}
	for i, field := range fields {
		switch field.Name() {
		case "__pgpq_key":
			layout.key = i
		case ds.Geometry:
			layout.geometry = i
		default:
			layout.types[i] = typeOf(ds.Columns, field.Name())
		}
	}
	return layout
}

func (l cellLayout) row(feature Feature) parquet.Row {
	row := make(parquet.Row, len(l.fields))
	for column := range l.fields {
		switch column {
		case l.key:
			row[column] = parquet.Int64Value(spatial.ToOrdered(feature.Key)).Level(0, 1, column)
		case l.geometry:
			row[column] = parquet.ByteArrayValue(feature.Geometry).Level(0, 1, column)
		default:
			value, ok := valueOf(l.types[column], feature.Props[l.fields[column].Name()])
			if !ok {
				row[column] = parquet.NullValue().Level(0, 0, column)
				continue
			}
			row[column] = value.Level(0, 1, column)
		}
	}
	return row
}

func FeatureBounds(features []Feature) [4]float64 {
	bounds := features[0].Bounds
	for _, feature := range features[1:] {
		bounds[0] = min(bounds[0], feature.Bounds[0])
		bounds[1] = min(bounds[1], feature.Bounds[1])
		bounds[2] = max(bounds[2], feature.Bounds[2])
		bounds[3] = max(bounds[3], feature.Bounds[3])
	}
	return bounds
}

func nodeOf(typ cql.Type) parquet.Node {
	switch typ {
	case cql.Bool:
		return parquet.Leaf(parquet.BooleanType)
	case cql.Int:
		return parquet.Leaf(parquet.Int64Type)
	case cql.Double:
		return parquet.Leaf(parquet.DoubleType)
	case cql.JSON:
		return parquet.JSON()
	}
	return parquet.String()
}

func typeOf(cols []Column, name string) cql.Type {
	for _, col := range cols {
		if col.Name == name {
			return col.Type
		}
	}
	return cql.String
}

func valueOf(typ cql.Type, value any) (parquet.Value, bool) {
	if value == nil {
		return parquet.Value{}, false
	}
	switch typ {
	case cql.Bool:
		if b, ok := value.(bool); ok {
			return parquet.BooleanValue(b), true
		}
	case cql.Int:
		switch n := value.(type) {
		case int:
			return parquet.Int64Value(int64(n)), true
		case int64:
			return parquet.Int64Value(n), true
		case float64:
			return parquet.Int64Value(int64(n)), true
		case json.Number:
			if v, err := n.Int64(); err == nil {
				return parquet.Int64Value(v), true
			}
		}
	case cql.Double:
		switch n := value.(type) {
		case float64:
			return parquet.DoubleValue(n), true
		case float32:
			return parquet.DoubleValue(float64(n)), true
		case int64:
			return parquet.DoubleValue(float64(n)), true
		}
	case cql.JSON:
		switch raw := value.(type) {
		case json.RawMessage:
			return parquet.ByteArrayValue(raw), true
		case []byte:
			return parquet.ByteArrayValue(raw), true
		case string:
			return parquet.ByteArrayValue([]byte(raw)), true
		}
	default:
		if s, ok := value.(string); ok {
			return parquet.ByteArrayValue([]byte(s)), true
		}
	}
	return parquet.Value{}, false
}

func geoMetadata(ds Dataset, bbox [4]float64) (string, error) {
	payload := map[string]any{
		"version":        "2.0.0",
		"primary_column": ds.Geometry,
		"columns": map[string]any{
			ds.Geometry: map[string]any{
				"encoding":       "WKB",
				"geometry_types": []string{},
				"crs":            nil,
				"bbox":           bbox[:],
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("geopq: metadata: %w", err)
	}
	return string(raw), nil
}
