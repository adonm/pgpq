package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

type exportFormat struct {
	ext       string
	mediaType string
	driver    string
	rustgis   bool
	gdalArgs  []string
}

var exportFormats = map[string]exportFormat{
	"fgb":     {ext: ".fgb", mediaType: "application/flatgeobuf"},
	"geojson": {ext: ".geojson", mediaType: "application/geo+json", driver: "GeoJSON", rustgis: true},
	"gpkg":    {ext: ".gpkg", mediaType: "application/geopackage+sqlite3", driver: "GPKG", rustgis: true},
	"parquet": {ext: ".parquet", mediaType: "application/vnd.apache.parquet", driver: "Parquet", rustgis: true},
	"shp":     {ext: ".zip", mediaType: "application/zip", driver: "ESRI Shapefile", rustgis: true},
	"kml":     {ext: ".kml", mediaType: "application/vnd.google-earth.kml+xml", driver: "KML", rustgis: true},
	"gpx":     {ext: ".gpx", mediaType: "application/gpx+xml", driver: "GPX", rustgis: true},
}

func formatList() string {
	names := make([]string, 0, len(exportFormats))
	for name := range exportFormats {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.Store.Collection(ctx, id); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	format := r.URL.Query().Get("format")
	spec, ok := exportFormats[format]
	if !ok {
		http.Error(w, "format must be one of "+formatList(), http.StatusBadRequest)
		return
	}
	if format != "fgb" && s.RustgisCLI == "" && s.OGR2OGR == "" {
		http.Error(w, "no converter configured", http.StatusNotImplemented)
		return
	}
	bbox, err := parseBBox(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := parseLimit(r, 1000000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter, err := requestFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	features, header, err := s.collectFeatures(ctx, id, bbox, r.URL.Query().Get("q"), filter, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(features) == 0 {
		http.Error(w, "no features matched", http.StatusNotFound)
		return
	}
	fgb, err := chunk.EncodeFeatures(id, header, features)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, mediaType, err := s.convertExport(ctx, fgb, format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+spec.ext))
	w.Write(body)
}

func (s *Server) convertExport(ctx context.Context, fgb []byte, format string) ([]byte, string, error) {
	spec, ok := exportFormats[format]
	if !ok {
		return nil, "", fmt.Errorf("unsupported format %q", format)
	}
	if format == "fgb" {
		return fgb, spec.mediaType, nil
	}
	dir, err := os.MkdirTemp("", "postfgb-export-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.fgb")
	out := filepath.Join(dir, "out"+spec.ext)
	if err := os.WriteFile(in, fgb, 0o600); err != nil {
		return nil, "", err
	}
	switch {
	case spec.rustgis && s.RustgisCLI != "":
		path, err := json.Marshal(out)
		if err != nil {
			return nil, "", err
		}
		command := fmt.Sprintf(`share.exportLayer={"path":%s}`, path)
		if err := runTool(ctx, s.RustgisCLI, "run", "--data", in, "--cmd", command); err != nil {
			return nil, "", err
		}
	case s.OGR2OGR != "":
		args := append([]string{"-f", spec.driver}, spec.gdalArgs...)
		args = append(args, out, in)
		if err := runTool(ctx, s.OGR2OGR, args...); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", errors.New("no converter configured for " + format)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		return nil, "", err
	}
	return body, spec.mediaType, nil
}

func runTool(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
