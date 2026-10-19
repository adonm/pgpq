package ingest

import (
	"errors"
	"fmt"
	"io"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
)

type Dataset struct {
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
	cols := chunk.HeaderColumns(header)
	idColumn := opts.IDColumn
	if idColumn == "" {
		idColumn = detectIDColumn(cols)
	} else if !hasColumn(cols, idColumn) {
		return Dataset{}, invalid(fmt.Errorf("id column %q is not in the schema", idColumn))
	}
	transform, err := reproject(header)
	if err != nil {
		return Dataset{}, invalid(err)
	}
	var features []chunk.InputFeature
	for {
		f, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Dataset{}, invalid(err)
		}
		feature, err := toInput(header, f, transform)
		if err != nil {
			return Dataset{}, invalid(err)
		}
		features = append(features, feature)
	}
	decideJSONColumns(cols, features)
	rewriteJSONValues(cols, features)
	var flat flattening
	if opts.Flatten {
		cols, flat = flattenJSONColumns(cols, features)
	}
	flat.apply(features)
	return Dataset{Columns: cols, IDColumn: idColumn, Features: features}, nil
}
