package vparquet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/parquet-go/parquet-go/format"
)

type geoDocument struct {
	document map[string]any
	columns  map[string]map[string]any
	primary  string
}

func parseGeo(value string) (*geoDocument, error) {
	var document map[string]any
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	version, _ := document["version"].(string)
	if !isGeoParquet2(version) {
		return nil, fmt.Errorf("version %q is not GeoParquet 2.0", version)
	}
	primary, _ := document["primary_column"].(string)
	if primary == "" {
		return nil, fmt.Errorf("primary_column is missing")
	}
	rawColumns, ok := document["columns"].(map[string]any)
	if !ok || len(rawColumns) == 0 {
		return nil, fmt.Errorf("columns is missing")
	}
	columns := make(map[string]map[string]any, len(rawColumns))
	for name, raw := range rawColumns {
		column, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("column %q is not an object", name)
		}
		encoding, _ := column["encoding"].(string)
		if !strings.EqualFold(encoding, "WKB") {
			return nil, fmt.Errorf("column %q encoding %q is not WKB", name, encoding)
		}
		columns[name] = column
	}
	if _, ok := columns[primary]; !ok {
		return nil, fmt.Errorf("primary column %q is not listed", primary)
	}
	return &geoDocument{document: document, columns: columns, primary: primary}, nil
}

func isGeoParquet2(version string) bool {
	return version == "2.0" || strings.HasPrefix(version, "2.0.")
}

func validateGeoGeometry(geo *geoDocument, geometry map[string]int) error {
	if _, ok := geometry[geo.primary]; !ok {
		return fmt.Errorf("primary column %q is not a native geometry column", geo.primary)
	}
	for name := range geo.columns {
		if _, ok := geometry[name]; !ok {
			return fmt.Errorf("column %q is not a native geometry column", name)
		}
	}
	return nil
}

func decodeBBox(value any) ([]float64, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("not an array")
	}
	switch len(values) {
	case 4, 6, 8:
	default:
		return nil, fmt.Errorf("has %d coordinates, want 4, 6 or 8", len(values))
	}
	box := make([]float64, len(values))
	for i := range values {
		number, ok := values[i].(float64)
		if !ok {
			return nil, fmt.Errorf("coordinate %d is not a number", i)
		}
		box[i] = number
	}
	return box, nil
}

func decodeGeometryTypes(value any) ([]string, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	types := make([]string, len(values))
	for i := range values {
		name, ok := values[i].(string)
		if !ok {
			return nil, false
		}
		types[i] = name
	}
	return types, true
}

func mergeGeoMetadata(shards []shardFile) (string, bool, error) {
	baseIndex := -1
	for i := range shards {
		if shards[i].geo != nil {
			baseIndex = i
			break
		}
	}
	if baseIndex < 0 {
		return "", false, nil
	}
	base := shards[baseIndex].geo
	for i := range shards {
		if i == baseIndex || shards[i].geo == nil {
			continue
		}
		if err := geoCompatible(base, shards[i].geo); err != nil {
			return "", false, invalid("shard %d geo metadata: %v", i, err)
		}
	}
	complete := true
	for i := range shards {
		if shards[i].geo == nil {
			complete = false
			break
		}
	}
	for name, column := range base.columns {
		mergeGeoBBox(name, column, shards, complete)
		mergeGeoTypes(name, column, shards, complete)
	}
	raw, err := json.Marshal(base.document)
	if err != nil {
		return "", false, invalid("merge geo metadata: %v", err)
	}
	return string(raw), true, nil
}

func geoCompatible(base, other *geoDocument) error {
	if base.primary != other.primary {
		return fmt.Errorf("primary column %q differs from %q", other.primary, base.primary)
	}
	if len(base.columns) != len(other.columns) {
		return fmt.Errorf("geometry column count differs")
	}
	for name, baseColumn := range base.columns {
		otherColumn, ok := other.columns[name]
		if !ok {
			return fmt.Errorf("column %q is missing", name)
		}
		if !reflect.DeepEqual(geoCRS(baseColumn), geoCRS(otherColumn)) {
			return fmt.Errorf("column %q crs differs", name)
		}
	}
	return nil
}

func geoCRS(column map[string]any) any {
	value, _ := column["crs"]
	return value
}

func mergeGeoBBox(name string, column map[string]any, shards []shardFile, complete bool) {
	baseBox, ok := geoBBox(column)
	if !ok || !complete {
		delete(column, "bbox")
		return
	}
	merged := slices.Clone(baseBox)
	for i := range shards {
		other := shards[i].geo
		if other == nil {
			delete(column, "bbox")
			return
		}
		box, ok := geoBBox(other.columns[name])
		if !ok || len(box) != len(merged) {
			delete(column, "bbox")
			return
		}
		unionBBox(merged, box)
		for _, statistics := range geometryStatistics(shards[i], name, len(merged)) {
			unionBBox(merged, statistics)
		}
	}
	column["bbox"] = merged
}

func mergeGeoTypes(name string, column map[string]any, shards []shardFile, complete bool) {
	baseTypes, ok := geoTypes(column)
	if !ok || len(baseTypes) == 0 {
		column["geometry_types"] = []string{}
		return
	}
	if !complete {
		column["geometry_types"] = []string{}
		return
	}
	merged := slices.Clone(baseTypes)
	for i := range shards {
		other := shards[i].geo
		if other == nil {
			column["geometry_types"] = []string{}
			return
		}
		types, ok := geoTypes(other.columns[name])
		if !ok || len(types) == 0 {
			column["geometry_types"] = []string{}
			return
		}
		for _, geometryType := range types {
			if !slices.Contains(merged, geometryType) {
				merged = append(merged, geometryType)
			}
		}
	}
	if merged == nil {
		merged = []string{}
	}
	column["geometry_types"] = merged
}

func geoBBox(column map[string]any) ([]float64, bool) {
	value, ok := column["bbox"]
	if !ok {
		return nil, false
	}
	switch box := value.(type) {
	case []float64:
		return box, true
	case []any:
		values, err := decodeBBox(box)
		if err != nil {
			return nil, false
		}
		return values, true
	}
	return nil, false
}

func geoTypes(column map[string]any) ([]string, bool) {
	return decodeGeometryTypes(column["geometry_types"])
}

func geometryStatistics(shard shardFile, name string, dimensions int) [][]float64 {
	leaf, ok := shard.geometry[name]
	if !ok {
		return nil
	}
	boxes := make([][]float64, 0)
	for i := range shard.meta.RowGroups {
		group := &shard.meta.RowGroups[i]
		if leaf >= len(group.Columns) {
			continue
		}
		if box, ok := statisticsBox(group.Columns[leaf].MetaData.GeospatialStatistics, dimensions); ok {
			boxes = append(boxes, box)
		}
	}
	return boxes
}

func statisticsBox(stats format.GeospatialStatistics, dimensions int) ([]float64, bool) {
	box := stats.BBox
	if box.XMin == 0 && box.XMax == 0 && box.YMin == 0 && box.YMax == 0 {
		return nil, false
	}
	switch dimensions {
	case 4:
		return []float64{box.XMin, box.YMin, box.XMax, box.YMax}, true
	case 6:
		zmin, okMin := box.ZMin.Get()
		zmax, okMax := box.ZMax.Get()
		if !okMin || !okMax {
			return nil, false
		}
		return []float64{box.XMin, box.YMin, zmin, box.XMax, box.YMax, zmax}, true
	case 8:
		zmin, okZMin := box.ZMin.Get()
		zmax, okZMax := box.ZMax.Get()
		mmin, okMMin := box.MMin.Get()
		mmax, okMMax := box.MMax.Get()
		if !okZMin || !okZMax || !okMMin || !okMMax {
			return nil, false
		}
		return []float64{box.XMin, box.YMin, zmin, mmin, box.XMax, box.YMax, zmax, mmax}, true
	}
	return nil, false
}

func unionBBox(destination, source []float64) {
	middle := len(destination) / 2
	for i := range destination {
		if i < middle {
			destination[i] = min(destination[i], source[i])
		} else {
			destination[i] = max(destination[i], source[i])
		}
	}
}
