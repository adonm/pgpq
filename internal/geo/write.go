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
	fields := schema.Fields()
	rows := make([]parquet.Row, len(features))
	bbox := features[0].Bounds
	for i, feature := range features {
		row := make(parquet.Row, 0, len(fields))
		for c, field := range fields {
			switch field.Name() {
			case "__pgpq_key":
				row = append(row, parquet.Int64Value(spatial.ToOrdered(feature.Key)).Level(0, 1, c))
			case ds.Geometry:
				row = append(row, parquet.ByteArrayValue(feature.Geometry).Level(0, 1, c))
			default:
				value, ok := valueOf(field.Name(), ds.Columns, feature.Props)
				if !ok {
					row = append(row, parquet.NullValue().Level(0, 0, c))
				} else {
					row = append(row, value.Level(0, 1, c))
				}
			}
		}
		rows[i] = row
		if i > 0 {
			bbox[0] = min(bbox[0], feature.Bounds[0])
			bbox[1] = min(bbox[1], feature.Bounds[1])
			bbox[2] = max(bbox[2], feature.Bounds[2])
			bbox[3] = max(bbox[3], feature.Bounds[3])
		}
	}
	var buf bytes.Buffer
	metadata, err := geoMetadata(ds, bbox)
	if err != nil {
		return nil, err
	}
	writer := parquet.NewWriter(&buf, &parquet.WriterConfig{
		Schema:             schema,
		MaxRowsPerRowGroup: rowGroupRows,
		Compression:        &zstd.Codec{Level: zstd.SpeedBetterCompression},
		KeyValueMetadata:   map[string]string{"geo": metadata},
	})
	if _, err := writer.WriteRows(rows); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

func valueOf(name string, cols []Column, props map[string]any) (parquet.Value, bool) {
	var typ cql.Type
	found := false
	for _, col := range cols {
		if col.Name == name {
			typ, found = col.Type, true
			break
		}
	}
	if !found {
		return parquet.Value{}, false
	}
	value, ok := props[name]
	if !ok || value == nil {
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
