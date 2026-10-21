package geo

import (
	"bytes"
	"errors"
	"io"
	"math"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"

	"github.com/adonm/pgpq/internal/spatial"
)

type Stats struct {
	Nulls int
	Min   *float64
	Max   *float64
}

func Open(data []byte) (*File, error) {
	file, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("geopq: cell file unreadable")
	}
	fields := file.Schema().Fields()
	f := &File{data: data, file: file, fields: fields, geometry: -1, key: -1}
	for i, field := range fields {
		switch field.Name() {
		case "__pgpq_key":
			f.key = i
			continue
		}
		if f.geometry < 0 && isGeometry(field) {
			f.geometry = i
			continue
		}
		f.attrs = append(f.attrs, Column{Name: field.Name(), Type: columnType(field)})
	}
	if f.geometry < 0 || f.key < 0 {
		return nil, errors.New("geopq: cell file lacks geometry or key")
	}
	return f, nil
}

func isGeometry(field parquet.Field) bool {
	logical := field.Type().LogicalType()
	if logical == nil {
		return false
	}
	switch logical.Value.(type) {
	case *format.GeometryType, *format.GeographyType:
		return true
	}
	return false
}

func (f *File) NumRows() int {
	return int(f.file.NumRows())
}

func (f *File) Attributes() []Column {
	return f.attrs
}

func (f *File) Geometry() string {
	return f.fields[f.geometry].Name()
}

func (f *File) GroupCount() int {
	return len(f.file.RowGroups())
}

func (f *File) GroupRows(group int) int {
	return int(f.file.RowGroups()[group].NumRows())
}

func (f *File) GroupBBox(group int) ([4]float64, bool) {
	metadata := f.file.Metadata().RowGroups
	if group < 0 || group >= len(metadata) || f.geometry >= len(metadata[group].Columns) {
		return [4]float64{}, false
	}
	stats := metadata[group].Columns[f.geometry].MetaData.GeospatialStatistics
	box := stats.BBox
	if box.XMin == 0 && box.XMax == 0 && box.YMin == 0 && box.YMax == 0 {
		return [4]float64{}, false
	}
	return [4]float64{box.XMin, box.YMin, box.XMax, box.YMax}, true
}

func (f *File) GroupStats(group int) map[string]Stats {
	metadata := f.file.Metadata().RowGroups
	out := make(map[string]Stats, len(f.attrs))
	if group < 0 || group >= len(metadata) {
		return out
	}
	for i, field := range f.fields {
		if i >= len(metadata[group].Columns) {
			break
		}
		stats := metadata[group].Columns[i].MetaData.Statistics
		item := Stats{Nulls: int(stats.NullCount)}
		if stats.MinValue != nil && stats.MaxValue != nil {
			switch field.Type().Kind() {
			case parquet.Int32, parquet.Int64:
				min, okMin := int64Value(stats.MinValue)
				max, okMax := int64Value(stats.MaxValue)
				if okMin && okMax {
					lo, hi := float64(min), float64(max)
					item.Min, item.Max = &lo, &hi
				}
			case parquet.Float, parquet.Double:
				min, okMin := float64Value(stats.MinValue)
				max, okMax := float64Value(stats.MaxValue)
				if okMin && okMax {
					item.Min, item.Max = &min, &max
				}
			}
		}
		out[field.Name()] = item
	}
	return out
}

func (f *File) KeyRange(group int) (int64, int64, bool) {
	metadata := f.file.Metadata().RowGroups
	if group < 0 || group >= len(metadata) || f.key >= len(metadata[group].Columns) {
		return 0, 0, false
	}
	stats := metadata[group].Columns[f.key].MetaData.Statistics
	if stats.MinValue == nil || stats.MaxValue == nil {
		return 0, 0, false
	}
	lo, okLo := int64Value(stats.MinValue)
	hi, okHi := int64Value(stats.MaxValue)
	return lo, hi, okLo && okHi
}

func (f *File) RowGroups(groups []int, idColumn string) ([]Feature, error) {
	var out []Feature
	for _, group := range groups {
		reader := parquet.NewRowGroupReader(f.file.RowGroups()[group], &parquet.ReaderConfig{Schema: f.file.Schema()})
		buf := make([]parquet.Row, 256)
		for {
			n, err := reader.ReadRows(buf)
			for i := 0; i < n; i++ {
				feature, err := f.fromRow(buf[i], idColumn)
				if err != nil {
					reader.Close()
					return nil, err
				}
				out = append(out, feature)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				reader.Close()
				return nil, err
			}
			if n == 0 {
				break
			}
		}
		reader.Close()
	}
	return out, nil
}

func (f *File) FeatureAt(row int, idColumn string) (Feature, error) {
	reader := parquet.NewReader(bytes.NewReader(f.data))
	defer reader.Close()
	if err := reader.SeekToRow(int64(row)); err != nil {
		return Feature{}, err
	}
	buf := make([]parquet.Row, 1)
	n, err := reader.ReadRows(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return Feature{}, err
	}
	if n == 0 {
		return Feature{}, errors.New("geopq: row not found")
	}
	return f.fromRow(buf[0], idColumn)
}

func (f *File) fromRow(row parquet.Row, idColumn string) (Feature, error) {
	var feature Feature
	for _, value := range row {
		column := value.Column()
		if column < 0 || column >= len(f.fields) || value.IsNull() {
			continue
		}
		field := f.fields[column]
		switch column {
		case f.geometry:
			feature.Geometry = append([]byte(nil), value.ByteArray()...)
		case f.key:
			feature.Key = spatial.FromOrdered(value.Int64())
		default:
			if feature.Props == nil {
				feature.Props = make(map[string]any)
			}
			feature.Props[field.Name()] = valueAny(columnType(field), value)
		}
	}
	bounds, ok := Bounds(feature.Geometry)
	if !ok {
		return Feature{}, errors.New("geopq: stored feature has no geometry")
	}
	feature.Bounds = bounds
	if raw, ok := feature.Props[idColumn]; ok {
		feature.ID, _ = canonical(raw)
	}
	return feature, nil
}

func int64Value(b []byte) (int64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return int64(uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56), true
}

func float64Value(b []byte) (float64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return math.Float64frombits(uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56), true
}

func (f *File) Scan(groups []int, idColumn string, yield func(Feature) bool) error {
	for _, group := range groups {
		reader := parquet.NewRowGroupReader(f.file.RowGroups()[group], &parquet.ReaderConfig{Schema: f.file.Schema()})
		buf := make([]parquet.Row, 256)
		for {
			n, err := reader.ReadRows(buf)
			for i := 0; i < n; i++ {
				feature, err := f.fromRow(buf[i], idColumn)
				if err != nil {
					reader.Close()
					return err
				}
				if !yield(feature) {
					reader.Close()
					return nil
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				reader.Close()
				return err
			}
			if n == 0 {
				break
			}
		}
		reader.Close()
	}
	return nil
}
