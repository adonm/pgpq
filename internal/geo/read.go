package geo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"

	"github.com/adonm/pgpq/internal/cql"
)

type Options struct {
	IDColumn string
}

func ReadDataset(r io.Reader, opts Options) (Dataset, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Dataset{}, err
	}
	file, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Dataset{}, invalid("GeoParquet file unreadable")
	}
	fields := file.Schema().Fields()
	geometry, name, err := geometryField(fields, file.Metadata())
	if err != nil {
		return Dataset{}, err
	}
	attrs := make([]Column, 0, len(fields)-1)
	attrIndex := make([]int, 0, len(fields)-1)
	for i, field := range fields {
		if i == geometry || field.Name() == "__pgpq_key" || field.Type().PhysicalType() == nil {
			continue
		}
		attrs = append(attrs, Column{Name: field.Name(), Type: columnType(field)})
		attrIndex = append(attrIndex, i)
	}
	idColumn := opts.IDColumn
	if idColumn == "" {
		idColumn = detectID(attrs)
	}
	if !hasColumn(attrs, idColumn) {
		return Dataset{}, invalid("the collection needs an id column")
	}
	reader := parquet.NewReader(bytes.NewReader(data))
	defer reader.Close()
	var features []Feature
	buf := make([]parquet.Row, 256)
	for {
		n, err := reader.ReadRows(buf)
		for i := 0; i < n; i++ {
			props := make(map[string]any, len(attrs))
			var geometryBytes []byte
			for _, value := range buf[i] {
				column := value.Column()
				if value.IsNull() {
					continue
				}
				if column == geometry {
					geometryBytes = append([]byte(nil), value.ByteArray()...)
					continue
				}
				if column < 0 {
					continue
				}
				for j, field := range fields {
					if field.Name() == "__pgpq_key" {
						continue
					}
					if j == column {
						props[field.Name()] = valueAny(columnType(field), value)
						break
					}
				}
			}
			if len(geometryBytes) == 0 {
				continue
			}
			bounds, ok := Bounds(geometryBytes)
			if !ok {
				return Dataset{}, invalid("feature has no usable geometry")
			}
			idValue, ok := props[idColumn]
			if !ok {
				return Dataset{}, invalid("feature has no id")
			}
			id, ok := canonical(idValue)
			if !ok || id == "" {
				return Dataset{}, invalid("feature has an unusable id")
			}
			features = append(features, Feature{Geometry: geometryBytes, Bounds: bounds, Props: props, ID: id})
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Dataset{}, invalid(err.Error())
		}
		if n == 0 {
			break
		}
	}
	return Dataset{Columns: attrs, Geometry: name, IDColumn: idColumn, Features: features}, nil
}

func geometryField(fields []parquet.Field, md *format.FileMetaData) (int, string, error) {
	for i, field := range fields {
		logical := field.Type().LogicalType()
		if logical == nil {
			continue
		}
		switch x := logical.Value.(type) {
		case *format.GeometryType:
			if err := checkCRS(x.CRS); err != nil {
				return 0, "", err
			}
			return i, field.Name(), nil
		case *format.GeographyType:
			if err := checkCRS(x.CRS); err != nil {
				return 0, "", err
			}
			return i, field.Name(), nil
		}
	}
	var metadata struct {
		PrimaryColumn string `json:"primary_column"`
		Columns       map[string]struct {
			Encoding string `json:"encoding"`
		} `json:"columns"`
	}
	if md != nil {
		for _, kv := range md.KeyValueMetadata {
			if kv.Key == "geo" && json.Unmarshal([]byte(kv.Value), &metadata) == nil && metadata.PrimaryColumn != "" {
				for i, field := range fields {
					if field.Name() == metadata.PrimaryColumn && field.Type().PhysicalType() != nil && field.Type().Kind() == parquet.ByteArray {
						return i, field.Name(), nil
					}
				}
			}
		}
	}
	return 0, "", invalid("a native Parquet GEOMETRY column is required")
}

func checkCRS(crs string) error {
	switch strings.TrimSpace(crs) {
	case "", "OGC:CRS84", "EPSG:4326", "urn:ogc:def:crs:OGC:1.3:CRS84":
		return nil
	}
	return invalid("only OGC:CRS84/EPSG:4326 geometries are accepted")
}

func columnType(field parquet.Field) cql.Type {
	if field.Type().PhysicalType() == nil {
		return cql.String
	}
	if logical := field.Type().LogicalType(); logical != nil {
		if _, ok := logical.Value.(*format.JsonType); ok {
			return cql.JSON
		}
	}
	switch field.Type().Kind() {
	case parquet.Boolean:
		return cql.Bool
	case parquet.Int32, parquet.Int64:
		return cql.Int
	case parquet.Float, parquet.Double:
		return cql.Double
	}
	return cql.String
}

func valueAny(typ cql.Type, value parquet.Value) any {
	switch value.Kind() {
	case parquet.Boolean:
		return value.Boolean()
	case parquet.Int32:
		return int64(value.Int32())
	case parquet.Int64:
		return value.Int64()
	case parquet.Float:
		return float64(value.Float())
	case parquet.Double:
		return value.Double()
	case parquet.ByteArray, parquet.FixedLenByteArray:
		raw := append([]byte(nil), value.ByteArray()...)
		if typ == cql.JSON {
			return json.RawMessage(raw)
		}
		return string(raw)
	}
	return nil
}

func canonical(value any) (string, bool) {
	switch x := value.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case json.Number:
		return x.String(), true
	}
	return "", false
}

func invalid(message string) error {
	return fmt.Errorf("geopq: %s", message)
}

var idNames = []string{"id", "fid", "gid", "ogc_fid"}

func detectID(cols []Column) string {
	for _, name := range idNames {
		for _, col := range cols {
			if strings.EqualFold(col.Name, name) {
				return col.Name
			}
		}
	}
	return ""
}

func hasColumn(cols []Column, name string) bool {
	for _, col := range cols {
		if col.Name == name {
			return true
		}
	}
	return false
}
