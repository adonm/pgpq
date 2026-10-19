package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/layer"
	"github.com/adonm/postfgb/internal/spatial"
	"github.com/adonm/postfgb/internal/store"
)

func (s *Server) putLayer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	if !store.ValidName(name) {
		http.Error(w, "invalid collection name", http.StatusUnprocessableEntity)
		return
	}
	var cells []uint64
	if r.URL.Query().Has("cells") {
		parsed, err := spatial.Cells(strings.Split(r.URL.Query().Get("cells"), ","))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		cells = parsed
	}
	ds, err := ingest.ReadDataset(r.Body, ingest.Options{
		IDColumn: r.URL.Query().Get("id_column"),
		Flatten:  flattenJSON(r),
	})
	if err != nil {
		layerError(w, err)
		return
	}
	result, err := s.layers.Load(r.Context(), name, ds, cells)
	if err != nil {
		layerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) layerItems(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bbox, err := parseBBox(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter, err := requestFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	level := 0
	if value := r.URL.Query().Get("overview"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			http.Error(w, "overview must be a level number", http.StatusBadRequest)
			return
		}
		level = parsed
	}
	q := layer.Query{BBox: bbox, Filter: filter, Level: level, Limit: limit}
	if value := r.URL.Query().Get("cursor"); value != "" {
		cursor, err := layer.DecodeCursor(value)
		if err != nil {
			layerError(w, err)
			return
		}
		q.Cursor = &cursor
	}
	page, err := s.layers.Items(r.Context(), r.PathValue("id"), q)
	if err != nil {
		layerError(w, err)
		return
	}
	next := layerNextLink(r, page.Next)
	wantFGB := r.URL.Query().Get("f") == "flatgeobuf" || strings.Contains(r.Header.Get("Accept"), "application/flatgeobuf")
	if wantFGB && len(page.Hits) > 0 {
		writeLayerFGB(w, r, page, next)
		return
	}
	var matched *int64
	if bbox == nil && level == 0 && r.URL.Query().Get("numberMatched") == "true" {
		n, err := s.layers.Count(r.Context(), r.PathValue("id"), page.Snapshot, filter)
		if err != nil {
			layerError(w, err)
			return
		}
		matched = &n
	}
	writeLayerGeoJSON(w, page, next, matched)
}

type layerFeatureCollection struct {
	Type           string          `json:"type"`
	Snapshot       int64           `json:"snapshot"`
	NumberMatched  *int64          `json:"numberMatched,omitempty"`
	NumberReturned int             `json:"numberReturned"`
	Features       []chunk.Feature `json:"features"`
	Links          []link          `json:"links,omitempty"`
}

func writeLayerGeoJSON(w http.ResponseWriter, page layer.Page, next string, matched *int64) {
	out := layerFeatureCollection{
		Type:           "FeatureCollection",
		Snapshot:       page.Snapshot,
		NumberMatched:  matched,
		NumberReturned: len(page.Hits),
		Features:       make([]chunk.Feature, 0, len(page.Hits)),
	}
	for _, hit := range page.Hits {
		feature := hit.Chunk.GeoJSON(hit.Feature)
		feature.ID = hit.ID
		out.Features = append(out.Features, *feature)
	}
	if next != "" {
		out.Links = []link{{Href: next, Rel: "next", Type: "application/geo+json"}}
	}
	w.Header().Set("Content-Type", "application/geo+json")
	json.NewEncoder(w).Encode(out)
}

func writeLayerFGB(w http.ResponseWriter, r *http.Request, page layer.Page, next string) {
	header := page.Hits[0].Chunk.Header()
	geometry := header.GeometryType()
	features := make([]*flattypes.Feature, len(page.Hits))
	for i, hit := range page.Hits {
		features[i] = hit.Feature
		if hit.Chunk.Header().GeometryType() != geometry {
			geometry = flattypes.GeometryTypeUnknown
		}
	}
	data, err := chunk.EncodeFeatures(page.Collection, header, geometry, features)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if next != "" {
		w.Header().Set("Link", "<"+next+">; rel=\"next\"")
	}
	w.Header().Set("Content-Type", "application/flatgeobuf")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func layerNextLink(r *http.Request, next *layer.Cursor) string {
	if next == nil {
		return ""
	}
	u := *r.URL
	query := u.Query()
	query.Set("cursor", next.String())
	u.RawQuery = query.Encode()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + u.RequestURI()
}

func layerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, layer.ErrInvalid):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, layer.ErrGone):
		http.Error(w, err.Error(), http.StatusGone)
	case errors.Is(err, layer.ErrBadCursor):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, layer.ErrFeatureNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, layer.ErrDuplicate):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "collection not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) layerItem(w http.ResponseWriter, r *http.Request) {
	hit, err := s.layers.Lookup(r.Context(), r.PathValue("id"), r.PathValue("featureID"))
	if err != nil {
		layerError(w, err)
		return
	}
	feature := hit.Chunk.GeoJSON(hit.Feature)
	feature.ID = hit.ID
	w.Header().Set("Content-Type", "application/geo+json")
	json.NewEncoder(w).Encode(feature)
}
