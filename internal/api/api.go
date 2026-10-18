package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

type Server struct {
	Store     *store.Store
	MaxChunks int
	MaxBytes  int
	ReadOnly  bool
	metrics   *metrics
	virtuals  *virtualCache
	chunks    *chunkByteCache
}

type Config struct {
	Store    *store.Store
	ReadOnly bool
	MaxBytes int
}

type link struct {
	Href      string `json:"href"`
	Rel       string `json:"rel"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title,omitempty"`
	Templated bool   `json:"templated,omitempty"`
}

const defaultLimit = 100000

func New(st *store.Store) http.Handler {
	return NewWithConfig(Config{Store: st})
}

func NewWithConfig(cfg Config) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("postfgb", "0.1.0"))
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 32 << 20
	}
	s := &Server{
		Store: cfg.Store, MaxChunks: 64, MaxBytes: cfg.MaxBytes,
		ReadOnly: cfg.ReadOnly, metrics: newMetrics(), virtuals: newVirtualCache(), chunks: newChunkByteCache(),
	}
	s.register(api)
	mux.HandleFunc("GET /{$}", s.landingPage)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.metricsHandler)
	mux.HandleFunc("GET /collections/{id}/items", s.items)
	mux.HandleFunc("GET /collections/{id}/items/{featureID}", s.feature)
	mux.HandleFunc("GET /collections/{id}/items.fgb", func(w http.ResponseWriter, r *http.Request) {
		if virtualRequest(r) {
			s.serveVirtualCollection(w, r)
			return
		}
		query := r.URL.Query()
		query.Set("f", "flatgeobuf")
		r.URL.RawQuery = query.Encode()
		s.items(w, r)
	})
	if !s.ReadOnly {
		mux.HandleFunc("PUT /collections/{id}", s.putCollection)
		mux.HandleFunc("DELETE /collections/{id}", s.deleteCollection)
	}
	mux.HandleFunc("GET /stac/collections", s.stacCollections)
	mux.HandleFunc("GET /stac/collections/{id}", s.stacCollection)
	mux.HandleFunc("GET /viewer", s.viewer)
	mux.HandleFunc("GET /viewer.rgis", s.viewerProject)
	mux.HandleFunc("GET /api", func(w http.ResponseWriter, r *http.Request) {
		body, err := api.OpenAPI().Downgrade()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.0")
		w.Write(body)
	})
	return corsMiddleware(compressionMiddleware(s.observe(rejectUnsupportedQuery(mux))))
}

func rejectUnsupportedQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		for _, name := range []string{"sortby", "q"} {
			if query.Has(name) {
				http.Error(w, name+" is not supported", http.StatusBadRequest)
				return
			}
		}
		for _, name := range []string{"crs", "bbox-crs"} {
			if value := query.Get(name); value != "" && value != crs84 {
				http.Error(w, name+" must be "+crs84, http.StatusBadRequest)
				return
			}
		}
		if value := query.Get("filter-lang"); value != "" && value != "cql2-text" {
			http.Error(w, "filter-lang must be cql2-text", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) register(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "conformance", Method: "GET", Path: "/conformance", Summary: "Conformance"}, s.conformance)
	huma.Register(api, huma.Operation{OperationID: "collections", Method: "GET", Path: "/collections", Summary: "Collections"}, s.collections)
	huma.Register(api, huma.Operation{OperationID: "collection", Method: "GET", Path: "/collections/{id}", Summary: "Collection"}, s.collection)
	huma.Register(api, huma.Operation{OperationID: "queryables", Method: "GET", Path: "/collections/{id}/queryables", Summary: "Queryables"}, s.queryables)
}

type landing struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Links       []link `json:"links"`
}

func (s *Server) landingPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(landing{
		Title:       "postfgb",
		Description: "OGC API Features over FlatGeobuf chunks stored in PostgreSQL",
		Links: []link{
			{Href: "/", Rel: "self", Type: "application/json", Title: "This document"},
			{Href: "/api", Rel: "service-desc", Type: "application/vnd.oai.openapi+json;version=3.0", Title: "API definition"},
			{Href: "/conformance", Rel: "conformance", Type: "application/json", Title: "Conformance classes"},
			{Href: "/collections", Rel: "data", Type: "application/json", Title: "Collections"},
			{Href: "/viewer", Rel: "http://www.opengis.net/def/rel/ogc/1.0/data", Type: "text/html", Title: "Map viewer"},
		},
	})
}

type conformanceOutput struct {
	Body struct {
		ConformsTo []string `json:"conformsTo"`
	}
}

func (s *Server) conformance(context.Context, *struct{}) (*conformanceOutput, error) {
	out := &conformanceOutput{}
	out.Body.ConformsTo = []string{
		"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/core",
		"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/oas30",
		"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/geojson",
		"http://www.opengis.net/spec/ogcapi-features-3/1.0/conf/filter",
		"http://www.opengis.net/spec/ogcapi-features-3/1.0/conf/features-filter",
		"http://www.opengis.net/spec/cql2/1.0/conf/cql2-text",
		"http://www.opengis.net/spec/cql2/1.0/conf/basic-cql2",
	}
	return out, nil
}

type spatialExtent struct {
	BBox [][]float64 `json:"bbox"`
}

type temporalExtent struct {
	Interval [][]string `json:"interval"`
}

type extent struct {
	Spatial  spatialExtent   `json:"spatial"`
	Temporal *temporalExtent `json:"temporal,omitempty"`
}

type collection struct {
	ID          string   `json:"id"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	ItemType    string   `json:"itemType"`
	CRS         []string `json:"crs"`
	Extent      *extent  `json:"extent,omitempty"`
	Links       []link   `json:"links"`
}

const crs84 = "http://www.opengis.net/def/crs/OGC/1.3/CRS84"

type collectionsOutput struct {
	Body struct {
		Collections []collection `json:"collections"`
		Links       []link       `json:"links"`
	}
}

func (s *Server) collections(ctx context.Context, _ *struct{}) (*collectionsOutput, error) {
	infos, err := s.Store.Collections(ctx)
	if err != nil {
		return nil, err
	}
	out := &collectionsOutput{}
	for _, info := range infos {
		out.Body.Collections = append(out.Body.Collections, s.collectionBody(ctx, info))
	}
	out.Body.Links = []link{{Href: "/collections", Rel: "self", Type: "application/json"}}
	return out, nil
}

type collectionInput struct {
	ID string `path:"id"`
}

type collectionOutput struct {
	Body collection
}

func (s *Server) collection(ctx context.Context, in *collectionInput) (*collectionOutput, error) {
	info, err := s.Store.Collection(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("collection not found")
	}
	if err != nil {
		return nil, err
	}
	return &collectionOutput{Body: s.collectionBody(ctx, info)}, nil
}

func (s *Server) collectionBody(ctx context.Context, info store.CollectionInfo) collection {
	title := info.Name
	if info.Title != nil && *info.Title != "" {
		title = *info.Title
	}
	body := collection{
		ID:       info.Name,
		Title:    title,
		Kind:     info.Kind,
		ItemType: "feature",
		CRS:      []string{crs84},
		Links: []link{
			{Href: "/collections/" + info.Name, Rel: "self", Type: "application/json"},
			{Href: "/collections/" + info.Name + "/items", Rel: "items", Type: "application/geo+json"},
			{Href: "/collections/" + info.Name + "/items?f=flatgeobuf", Rel: "alternate", Type: "application/flatgeobuf", Title: "Items (FlatGeobuf)"},
		},
	}
	if info.Description != nil {
		body.Description = *info.Description
	}
	body.Extent = s.collectionExtent(ctx, info)
	return body
}

func (s *Server) collectionExtent(_ context.Context, info store.CollectionInfo) *extent {
	if info.MinX == nil || info.MinY == nil || info.MaxX == nil || info.MaxY == nil {
		return nil
	}
	out := &extent{Spatial: spatialExtent{
		BBox: [][]float64{{*info.MinX, *info.MinY, *info.MaxX, *info.MaxY}},
	}}
	if info.TimeMin != nil && info.TimeMax != nil {
		out.Temporal = timeInterval(*info.TimeMin, *info.TimeMax)
	}
	return out
}

func timeInterval(minT, maxT time.Time) *temporalExtent {
	return &temporalExtent{Interval: [][]string{{minT.UTC().Format(time.RFC3339), maxT.UTC().Format(time.RFC3339)}}}
}

func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	info, err := s.Store.Collection(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flatgeobuf := r.URL.Query().Get("f") == "flatgeobuf" ||
		strings.Contains(r.Header.Get("Accept"), "application/flatgeobuf")
	limit, err := parseLimit(r, defaultLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q, err := parseFeatureQuery(r, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q.offset = offset
	if flatgeobuf {
		q.overview = nil
		s.writeFGB(w, r, info, q, limit)
		return
	}
	var matched *int64
	if r.URL.Query().Get("numberMatched") == "true" {
		count, ok, err := s.countMatches(ctx, info, q)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			matched = &count
		}
	}
	w.Header().Set("Content-Type", "application/geo+json")
	if r.URL.Query().Get("crs") != "" {
		w.Header().Set("Content-Crs", crs84)
	}
	page := newPageWriter(w, limit, matched)
	if err := s.walkFeatures(ctx, info, q, page); err != nil {
		slog.Error("items walk failed", "collection", id, "error", err)
	}
	page.close(pageLink(r, limit, offset, page.count))
}

func (s *Server) writeFGB(w http.ResponseWriter, r *http.Request, info store.CollectionInfo, q featureQuery, limit int) {
	out := &fgbCollector{limit: limit}
	if err := s.walkFeatures(r.Context(), info, q, out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(out.features) == 0 {
		w.Header().Set("Content-Type", "application/geo+json")
		newPageWriter(w, limit, nil).close("")
		return
	}
	data, err := chunk.EncodeFeatures(info.Name, out.header, out.features)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/flatgeobuf")
	if link := pageLink(r, limit, q.offset, len(out.features)); link != "" {
		w.Header().Set("Link", "<"+link+">; rel=\"next\"")
	}
	w.Write(data)
}

type pageWriter struct {
	out   *bufio.Writer
	count int
	limit int
}

func newPageWriter(w http.ResponseWriter, limit int, matched *int64) *pageWriter {
	p := &pageWriter{out: bufio.NewWriter(w), limit: limit}
	p.out.WriteString(`{"type":"FeatureCollection"`)
	if matched != nil {
		p.out.WriteString(`,"numberMatched":`)
		p.out.WriteString(strconv.FormatInt(*matched, 10))
	}
	p.out.WriteString(`,"features":[`)
	return p
}

func (p *pageWriter) add(h hit) error {
	body, err := json.Marshal(h.geo)
	if err != nil {
		return nil
	}
	if p.count > 0 {
		p.out.WriteByte(',')
	}
	p.out.Write(body)
	p.count++
	return nil
}

func (p *pageWriter) remaining() int {
	return p.limit - p.count
}

func (p *pageWriter) close(next string) {
	p.out.WriteString(`],"numberReturned":`)
	p.out.WriteString(strconv.Itoa(p.count))
	if next != "" {
		body, _ := json.Marshal(next)
		p.out.WriteString(`,"links":[{"href":`)
		p.out.Write(body)
		p.out.WriteString(`,"rel":"next","type":"application/geo+json"}]`)
	}
	p.out.WriteString("}")
	p.out.Flush()
}

type fgbCollector struct {
	limit    int
	header   *flattypes.Header
	last     *chunk.Chunk
	features []*flattypes.Feature
}

func (f *fgbCollector) add(h hit) error {
	if h.chunk != f.last {
		header := h.chunk.Header()
		if f.header == nil {
			f.header = header
		} else if !sameColumns(f.header, header) {
			return errors.New("collection chunks have differing schemas")
		}
		f.last = h.chunk
	}
	f.features = append(f.features, h.raw)
	return nil
}

func (f *fgbCollector) remaining() int {
	return f.limit - len(f.features)
}

func sameColumns(a, b *flattypes.Header) bool {
	if a.ColumnsLength() != b.ColumnsLength() {
		return false
	}
	for i := 0; i < a.ColumnsLength(); i++ {
		var ca, cb flattypes.Column
		if !a.Columns(&ca, i) || !b.Columns(&cb, i) {
			return false
		}
		if string(ca.Name()) != string(cb.Name()) || ca.Type() != cb.Type() {
			return false
		}
	}
	return true
}

func pageLink(r *http.Request, limit, offset, count int) string {
	if count < limit {
		return ""
	}
	query := r.URL.Query()
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset+count))
	return r.URL.Path + "?" + query.Encode()
}
