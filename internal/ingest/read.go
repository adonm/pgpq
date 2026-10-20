package ingest

import (
	"errors"
	"fmt"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
)

var ErrInvalid = errors.New("invalid upload")

type Options struct {
	IDColumn string
}

func invalid(err error) error {
	return fmt.Errorf("%w: %v", ErrInvalid, err)
}

func wgs84(header *flattypes.Header) error {
	var crs flattypes.Crs
	if header.Crs(&crs) == nil {
		return nil
	}
	if code := int(crs.Code()); code > 0 && code != 4326 {
		return fmt.Errorf("unsupported CRS EPSG:%d; upload EPSG:4326", code)
	}
	if text := string(crs.CodeString()); text != "" && !strings.Contains(text, "4326") {
		return errors.New("upload must be EPSG:4326")
	}
	return nil
}

var idNames = []string{"id", "fid", "gid", "ogc_fid"}

func detectIDColumn(cols []chunk.Column) string {
	for _, name := range idNames {
		for _, col := range cols {
			if strings.EqualFold(col.Name, name) {
				return col.Name
			}
		}
	}
	return ""
}

func hasColumn(cols []chunk.Column, name string) bool {
	for _, col := range cols {
		if col.Name == name {
			return true
		}
	}
	return false
}
