package ingest

import (
	"errors"
	"fmt"
	"io"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
)

type Dataset struct {
	Header   *flattypes.Header
	Columns  []chunk.Column
	IDColumn string
	Features []chunk.InputFeature
}

func ReadDataset(body io.Reader, opts Options) (Dataset, error) {
	reader, err := fgb.NewReader(body)
	if err != nil {
		return Dataset{}, invalid(err)
	}
	header := reader.Header()
	if err := wgs84(header); err != nil {
		return Dataset{}, invalid(err)
	}
	cols := chunk.HeaderColumns(header)
	idColumn := opts.IDColumn
	if idColumn == "" {
		idColumn = detectIDColumn(cols)
	} else if !hasColumn(cols, idColumn) {
		return Dataset{}, invalid(fmt.Errorf("id column %q is not in the schema", idColumn))
	}
	var features []chunk.InputFeature
	for {
		f, raw, err := reader.NextRaw()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Dataset{}, invalid(err)
		}
		bounds, ok := chunk.Bounds(header, f)
		if !ok {
			return Dataset{}, invalid(errors.New("feature has no usable geometry"))
		}
		features = append(features, chunk.InputFeature{
			Raw:        raw,
			Bounds:     bounds,
			Properties: chunk.DecodePropertyText(f.PropertiesBytes(), cols),
		})
	}
	return Dataset{Header: header, Columns: cols, IDColumn: idColumn, Features: features}, nil
}
