package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/wroge/wgs84/v2"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
)

var crsDigits = regexp.MustCompile(`\d{3,6}`)

func crsCode(value string) (int, error) {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "crs84") {
		return 4326, nil
	}
	matches := crsDigits.FindAllString(lower, -1)
	if len(matches) == 0 {
		return 0, errors.New("invalid crs " + value)
	}
	code, err := strconv.Atoi(matches[len(matches)-1])
	if err != nil || code <= 0 {
		return 0, errors.New("invalid crs " + value)
	}
	return code, nil
}

func (s *Server) outputCRS(r *http.Request) (wgs84.Func, string, error) {
	value := r.URL.Query().Get("crs")
	if value == "" {
		return nil, "", nil
	}
	code, err := crsCode(value)
	if err != nil {
		return nil, "", err
	}
	if code == 4326 {
		return nil, "", nil
	}
	target, ok := wgs84.EPSG[code]
	if !ok {
		return nil, "", fmt.Errorf("unsupported crs EPSG:%d", code)
	}
	fn, err := wgs84.Transform(wgs84.EPSG[4326], target)
	if err != nil {
		return nil, "", err
	}
	return fn, fmt.Sprintf("http://www.opengis.net/def/crs/EPSG/0/%d", code), nil
}

func (s *Server) inputBBox(r *http.Request, bbox *[4]float64) (*[4]float64, error) {
	if bbox == nil {
		return nil, nil
	}
	value := r.URL.Query().Get("bbox-crs")
	if value == "" {
		return bbox, nil
	}
	code, err := crsCode(value)
	if err != nil {
		return nil, err
	}
	if code == 4326 {
		return bbox, nil
	}
	source, ok := wgs84.EPSG[code]
	if !ok {
		return nil, fmt.Errorf("unsupported bbox-crs EPSG:%d", code)
	}
	fn, err := wgs84.Transform(source, wgs84.EPSG[4326])
	if err != nil {
		return nil, err
	}
	x0, y0, _, err := fn(bbox[0], bbox[1], 0)
	if err != nil {
		return nil, err
	}
	x1, y1, _, err := fn(bbox[2], bbox[3], 0)
	if err != nil {
		return nil, err
	}
	out := [4]float64{min(x0, x1), min(y0, y1), max(x0, x1), max(y0, y1)}
	return &out, nil
}

type crsWriter struct {
	base featureSink
	fn   wgs84.Func
}

func (w *crsWriter) add(g *chunk.Feature) bool {
	if g.Geometry != nil {
		if err := ingest.TransformGeometry(g.Geometry, w.fn); err != nil {
			return true
		}
	}
	return w.base.add(g)
}

func (w *crsWriter) full() bool {
	return w.base.full()
}

func (w *crsWriter) remaining() int {
	return w.base.remaining()
}
