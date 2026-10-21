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
)

const preallocateRows = 1 << 20

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
	geometry, name, err := geometryField(fields)
	if err != nil {
		return Dataset{}, err
	}
	off := make([]int, len(fields))
	for i := range off {
		off[i] = -1
	}
	types := make([]Type, len(fields))
	attrs := make([]Column, 0, len(fields)-1)
	key := -1
	for i, field := range fields {
		switch {
		case i == geometry:
			continue
		case field.Name() == "__pgpq_key":
			key = i
			continue
		case field.Type().PhysicalType() == nil:
			continue
		}
		off[i] = len(attrs)
		types[i] = columnType(field)
		attrs = append(attrs, Column{Name: field.Name(), Type: types[i]})
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
	features := make([]Feature, 0, max(0, min(file.NumRows(), preallocateRows)))
	buf := make([]parquet.Row, 256)
	for {
		n, err := reader.ReadRows(buf)
		for i := 0; i < n; i++ {
			props := make(map[string]any, len(attrs))
			var geometryBytes []byte
			for _, value := range buf[i] {
				column := int(value.Column())
				if column < 0 || column >= len(fields) || column == key || value.IsNull() {
					continue
				}
				if column == geometry {
					geometryBytes = append([]byte(nil), value.ByteArray()...)
					continue
				}
				if attr := off[column]; attr >= 0 {
					props[attrs[attr].Name] = valueAny(types[column], value)
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

func geometryField(fields []parquet.Field) (int, string, error) {
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
	return 0, "", invalid("a native Parquet GEOMETRY column is required")
}

func checkCRS(crs string) error {
	crs = strings.TrimSpace(crs)
	switch crs {
	case "", "OGC:CRS84", "EPSG:4326", "urn:ogc:def:crs:OGC:1.3:CRS84":
		return nil
	}
	if strings.HasPrefix(crs, "{") {
		var proj struct {
			Name string `json:"name"`
			ID   struct {
				Authority string          `json:"authority"`
				Code      json.RawMessage `json:"code"`
			} `json:"id"`
		}
		if json.Unmarshal([]byte(crs), &proj) == nil {
			authority := strings.ToUpper(proj.ID.Authority)
			code := strings.Trim(string(proj.ID.Code), `"`)
			if (authority == "OGC" && code == "CRS84") || (authority == "EPSG" && code == "4326") {
				return nil
			}
		}
	}
	return invalid("only OGC:CRS84/EPSG:4326 geometries are accepted")
}

func columnType(field parquet.Field) Type {
	if field.Type().PhysicalType() == nil {
		return String
	}
	if logical := field.Type().LogicalType(); logical != nil {
		if _, ok := logical.Value.(*format.JsonType); ok {
			return JSON
		}
	}
	switch field.Type().Kind() {
	case parquet.Boolean:
		return Bool
	case parquet.Int32, parquet.Int64:
		return Int
	case parquet.Float, parquet.Double:
		return Double
	}
	return String
}

func valueAny(typ Type, value parquet.Value) any {
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
		if typ == JSON {
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
