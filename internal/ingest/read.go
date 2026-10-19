package ingest

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/wroge/wgs84/v2"

	"github.com/adonm/postfgb/internal/chunk"
)

var ErrInvalid = errors.New("invalid upload")

type Options struct {
	IDColumn string
	Flatten  bool
}

func invalid(err error) error {
	return fmt.Errorf("%w: %v", ErrInvalid, err)
}

func toInput(header *flattypes.Header, f *flattypes.Feature, transform wgs84.Func) (chunk.InputFeature, error) {
	shape, err := chunk.ParseFlat(header, f)
	if err != nil {
		return chunk.InputFeature{}, err
	}
	if shape == nil {
		return chunk.InputFeature{}, errors.New("feature missing geometry")
	}
	if transform != nil {
		if err := shape.Transform(transform); err != nil {
			return chunk.InputFeature{}, err
		}
	}
	return chunk.InputFeature{
		Parsed:     shape,
		Properties: chunk.DecodePropertyText(f.PropertiesBytes(), chunk.HeaderColumns(header)),
	}, nil
}

var (
	epsgCodeString = regexp.MustCompile(`(?i)epsg:\s*:?\s*(\d{3,6})`)
	epsgWkt        = regexp.MustCompile(`(?i)AUTHORITY\s*\[\s*"EPSG"\s*,\s*"?(\d{3,6})`)
)

func reproject(header *flattypes.Header) (wgs84.Func, error) {
	code, present, err := crsCode(header)
	if err != nil {
		return nil, err
	}
	if !present || code == 4326 {
		return nil, nil
	}
	from, ok := wgs84.EPSG[code]
	if !ok {
		return nil, fmt.Errorf("unsupported CRS EPSG:%d", code)
	}
	return wgs84.Transform(from, wgs84.EPSG[4326])
}

func crsCode(header *flattypes.Header) (int, bool, error) {
	var crs flattypes.Crs
	if header.Crs(&crs) == nil {
		return 0, false, nil
	}
	if code := int(crs.Code()); code > 0 && strings.EqualFold(string(crs.Org()), "EPSG") {
		return code, true, nil
	}
	for _, value := range []string{string(crs.CodeString()), string(crs.Wkt())} {
		if value == "" {
			continue
		}
		for _, re := range []*regexp.Regexp{epsgCodeString, epsgWkt} {
			if m := re.FindStringSubmatch(value); m != nil {
				if code, err := strconv.Atoi(m[1]); err == nil && code > 0 {
					return code, true, nil
				}
			}
		}
	}
	return 0, false, errors.New("cannot determine the CRS of the upload")
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
