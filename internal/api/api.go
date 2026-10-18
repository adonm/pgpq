package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

type Server struct {
	Store        *store.Store
	MaxChunks    int
	MaxBytes     int
	DefaultLimit int
	ReadOnly     bool
	metrics      *metrics
	virtuals     *virtualCache
	chunks       *chunkByteCache
	sweepMu      sync.Mutex
	lastSweep    time.Time
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
		Store: cfg.Store, MaxChunks: 64, MaxBytes: cfg.MaxBytes, DefaultLimit: 100000,
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
			{Href: "/stac", Rel: "stac", Type: "application/json", Title: "STAC catalog"},
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

var reservedParams = map[string]bool{
	"bbox": true, "limit": true, "offset": true, "f": true,
	"filter": true, "filter-lang": true, "format": true,
	"overview": true, "numberMatched": true,
	"datetime": true, "crs": true, "bbox-crs": true,
	"z": true, "x": true, "y": true,
}

func parseOverview(r *http.Request) (*int, error) {
	switch value := r.URL.Query().Get("overview"); value {
	case "", "auto":
		return nil, nil
	case "0", "1", "2":
		n, _ := strconv.Atoi(value)
		return &n, nil
	default:
		return nil, errors.New("overview must be auto, 0, 1 or 2")
	}
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
	defaultLimit := s.DefaultLimit
	if flatgeobuf {
		defaultLimit = flatgeobufLimit
	}
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
	q, err := s.parseFeatureQuery(r, info, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var matched *int64
	if r.URL.Query().Get("numberMatched") == "true" {
		count, ok, err := s.countMatches(ctx, info, q.bbox, q.filter, q.tw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			matched = &count
		}
	}
	if flatgeobuf {
		s.writeFGB(ctx, w, r, id, q.bbox, q.filter, q.offset, q.limit, 0)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	if r.URL.Query().Get("crs") != "" {
		w.Header().Set("Content-Crs", crs84)
	}
	fw := newFeatureWriter(w, limit, matched)
	s.streamCollection(ctx, fw, info, q)
	fw.close(pageLink(r, limit, offset, fw.count))
}

// walkChunks streams a collection's tier chunks in feature order, consuming the
// paging offset before handing each chunk's remaining features to visit, which
// returns false to stop. It reports how many features the offset consumed.
func (s *Server) walkChunks(ctx context.Context, info store.CollectionInfo, bbox *[4]float64, offset, tier int, visit func(store.ChunkMeta, *chunk.Chunk, []*flattypes.Feature, int) bool) int {
	skipped := 0
	after := int64(0)
	fallback := tier > 0
	for {
		metas, err := s.Store.ChunkMetas(ctx, info.Name, bbox, s.MaxChunks, tier, after)
		if err != nil {
			slog.Error("chunk metas query failed", "collection", info.Name, "error", err)
			return skipped
		}
		if len(metas) == 0 {
			if fallback && after == 0 {
				tier = 0
				fallback = false
				continue
			}
			return skipped
		}
		fallback = false
		metas, trimmed := budgetMetas(metas, s.MaxBytes)
		fetch := make([]store.ChunkMeta, 0, len(metas))
		fetchIDs := make([]int64, 0, len(metas))
		for _, meta := range metas {
			if bbox == nil && skipped+meta.FeatureCount <= offset {
				skipped += meta.FeatureCount
				continue
			}
			fetch = append(fetch, meta)
			fetchIDs = append(fetchIDs, meta.ID)
		}
		rows := s.fetchRows(ctx, info.Name, fetchIDs)
		for j, meta := range fetch {
			opened := s.openAndSearch(ctx, info, meta, rows[j])
			if opened.err != nil {
				slog.Error("chunk read failed", "chunk", meta.ID, "error", opened.err)
				continue
			}
			from := 0
			for from < len(opened.features) && skipped < offset {
				skipped++
				from++
			}
			if from == len(opened.features) {
				continue
			}
			if !visit(meta, opened.chunk, opened.features, from) {
				return skipped
			}
		}
		if len(metas) < s.MaxChunks && !trimmed {
			return skipped
		}
		after = metas[len(metas)-1].ID
	}
}

func (s *Server) streamBBox(ctx context.Context, sink featureSink, info store.CollectionInfo, bbox *[4]float64, offset, tier int) int {
	return s.walkChunks(ctx, info, bbox, offset, tier, func(meta store.ChunkMeta, c *chunk.Chunk, features []*flattypes.Feature, from int) bool {
		for i, g := range geojsonAll(c, features) {
			if i < from {
				continue
			}
			if !inBounds(g, bbox) {
				continue
			}
			g.ID = featureID(info, meta.ID, i, g.Properties)
			if !sink.add(g) {
				return false
			}
		}
		return true
	})
}

func (s *Server) streamMatches(ctx context.Context, sink featureSink, info store.CollectionInfo, bbox *[4]float64, filter cql.Expr, offset int) {
	matches, err := s.Store.Matches(ctx, info.Name, bbox, filter, sink.remaining(), offset)
	if err != nil {
		slog.Error("matches query failed", "collection", info.Name, "error", err)
		return
	}
	s.emitMatches(ctx, sink, info, bbox, matches)
}

func (s *Server) emitMatches(ctx context.Context, sink featureSink, info store.CollectionInfo, bbox *[4]float64, matches []store.FeatureMatch) {
	ords := map[int64]map[int]bool{}
	var ids []int64
	for _, m := range matches {
		set := ords[m.ChunkID]
		if set == nil {
			set = map[int]bool{}
			ords[m.ChunkID] = set
			ids = append(ids, m.ChunkID)
		}
		set[m.Ord] = true
	}
	for _, batch := range s.chunkBatches(ctx, info.Name, ids) {
		rows := s.fetchRows(ctx, info.Name, batch)
		for j := range rows {
			if rows[j].Data == nil {
				continue
			}
			row := rows[j]
			c, err := chunk.Open(row.Data)
			if err != nil {
				slog.Error("chunk open failed", "chunk", row.ID, "error", err)
				continue
			}
			features, err := c.SearchAll()
			if err != nil {
				slog.Error("chunk search failed", "chunk", row.ID, "error", err)
				continue
			}
			set := ords[row.ID]
			for i, f := range features {
				if !set[i] {
					continue
				}
				g := c.GeoJSON(f)
				if !inBounds(g, bbox) {
					continue
				}
				g.ID = featureID(info, row.ID, i, g.Properties)
				if !sink.add(g) {
					return
				}
			}
		}
	}
}

type featureSink interface {
	add(f *chunk.Feature) bool
	full() bool
	remaining() int
}

func (fw *featureWriter) full() bool {
	return fw.count >= fw.limit
}

func (fw *featureWriter) remaining() int {
	return fw.limit - fw.count
}

type featureWriter struct {
	out   *bufio.Writer
	count int
	limit int
	first bool
}

func newFeatureWriter(w http.ResponseWriter, limit int, matched *int64) *featureWriter {
	fw := &featureWriter{out: bufio.NewWriter(w), limit: limit, first: true}
	fw.out.WriteString(`{"type":"FeatureCollection"`)
	if matched != nil {
		fw.out.WriteString(`,"numberMatched":`)
		fw.out.WriteString(strconv.FormatInt(*matched, 10))
	}
	fw.out.WriteString(`,"features":[`)
	return fw
}

func (fw *featureWriter) add(f *chunk.Feature) bool {
	if fw.count >= fw.limit {
		return false
	}
	body, err := json.Marshal(f)
	if err != nil {
		return true
	}
	if !fw.first {
		fw.out.WriteByte(',')
	}
	fw.out.Write(body)
	fw.first = false
	fw.count++
	return fw.count < fw.limit
}

func (fw *featureWriter) close(next string) {
	fw.out.WriteString(`],"numberReturned":`)
	fw.out.WriteString(strconv.Itoa(fw.count))
	if next != "" {
		body, err := json.Marshal(next)
		if err == nil {
			fw.out.WriteString(`,"links":[{"href":`)
			fw.out.Write(body)
			fw.out.WriteString(`,"rel":"next","type":"application/geo+json"}]`)
		}
	}
	fw.out.WriteString("}")
	fw.out.Flush()
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

func parseFilters(r *http.Request) map[string]string {
	filters := map[string]string{}
	for name, values := range r.URL.Query() {
		if reservedParams[name] || len(values) == 0 {
			continue
		}
		filters[name] = values[0]
	}
	return filters
}

func requestFilter(r *http.Request) (cql.Expr, error) {
	var expr cql.Expr
	for name, value := range parseFilters(r) {
		expr = cql.And(expr, &cql.Compare{Prop: name, Op: "=", Value: typedLiteral(value)})
	}
	text := r.URL.Query().Get("filter")
	if text == "" {
		return expr, nil
	}
	parsed, err := cql.Parse(text)
	if err != nil {
		return nil, err
	}
	return cql.And(expr, parsed), nil
}

func typedLiteral(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}
	return value
}

func (s *Server) countMatches(ctx context.Context, info store.CollectionInfo, bbox *[4]float64, filter cql.Expr, tw timeWindow) (int64, bool, error) {
	if info.Kind == "view" {
		var total int64
		exact := true
		for _, member := range s.streamTargets(ctx, info) {
			count, ok, err := s.countMatches(ctx, member, bbox, filter, tw)
			if err != nil {
				return 0, false, err
			}
			if !ok {
				exact = false
				continue
			}
			total += count
		}
		return total, exact, nil
	}
	if tw.set {
		if info.TimeColumn == nil || *info.TimeColumn == "" {
			return 0, false, nil
		}
		filter = cql.And(filter, datetimeExpr(*info.TimeColumn, tw))
	}
	if filter != nil {
		count, err := s.Store.CountMatches(ctx, info.Name, bbox, filter)
		if err != nil {
			return 0, false, err
		}
		return count, true, nil
	}
	if bbox == nil {
		return info.FeatureCount, true, nil
	}
	return 0, false, nil
}

func featureID(info store.CollectionInfo, chunkID int64, ord int, props map[string]any) any {
	if info.IDColumn != nil {
		if value, ok := props[*info.IDColumn]; ok && value != nil {
			return value
		}
	}
	return fmt.Sprintf("%d-%d", chunkID, ord)
}

func inBounds(f *chunk.Feature, bbox *[4]float64) bool {
	if bbox == nil {
		return true
	}
	geom := chunk.ToOrb(f.Geometry)
	if geom == nil {
		return false
	}
	bound := geom.Bound()
	return bound.Max[0] >= bbox[0] && bound.Min[0] <= bbox[2] &&
		bound.Max[1] >= bbox[1] && bound.Min[1] <= bbox[3]
}

func (s *Server) writeFGB(ctx context.Context, w http.ResponseWriter, r *http.Request, id string, bbox *[4]float64, filter cql.Expr, offset, limit, tier int) {
	features, header, err := s.collectFeatures(ctx, id, bbox, filter, offset, limit, tier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(features) == 0 {
		w.Header().Set("Content-Type", "application/geo+json")
		fw := newFeatureWriter(w, limit, nil)
		fw.close("")
		return
	}
	data, err := chunk.EncodeFeatures(id, header, features)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/flatgeobuf")
	if link := pageLink(r, limit, offset, len(features)); link != "" {
		w.Header().Set("Link", "<"+link+">; rel=\"next\"")
	}
	w.Write(data)
}

func (s *Server) collectFeatures(ctx context.Context, id string, bbox *[4]float64, filter cql.Expr, offset, limit, tier int) ([]*flattypes.Feature, *flattypes.Header, error) {
	if info, err := s.Store.Collection(ctx, id); err == nil && info.Kind == "view" {
		return s.collectView(ctx, info, bbox, filter, offset, limit)
	}
	if filter != nil {
		return s.collectMatches(ctx, id, bbox, filter, offset, limit)
	}
	var features []*flattypes.Feature
	var header *flattypes.Header
	info, err := s.Store.Collection(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	s.walkChunks(ctx, info, bbox, offset, 0, func(_ store.ChunkMeta, c *chunk.Chunk, feats []*flattypes.Feature, from int) bool {
		if header == nil {
			header = c.Header()
		} else if !sameColumns(header, c.Header()) {
			return false
		}
		for i := from; i < len(feats) && len(features) < limit; i++ {
			g := c.GeoJSON(feats[i])
			if !inBounds(g, bbox) {
				continue
			}
			features = append(features, feats[i])
		}
		return len(features) < limit
	})
	return features, header, nil
}

func (s *Server) collectMatches(ctx context.Context, id string, bbox *[4]float64, filter cql.Expr, offset, limit int) ([]*flattypes.Feature, *flattypes.Header, error) {
	matches, err := s.Store.Matches(ctx, id, bbox, filter, limit, offset)
	if err != nil {
		return nil, nil, err
	}
	return s.collectMatchRows(ctx, id, bbox, matches)
}

func (s *Server) collectMatchRows(ctx context.Context, id string, bbox *[4]float64, matches []store.FeatureMatch) ([]*flattypes.Feature, *flattypes.Header, error) {
	ords := map[int64]map[int]bool{}
	var ids []int64
	for _, m := range matches {
		set := ords[m.ChunkID]
		if set == nil {
			set = map[int]bool{}
			ords[m.ChunkID] = set
			ids = append(ids, m.ChunkID)
		}
		set[m.Ord] = true
	}
	var features []*flattypes.Feature
	var header *flattypes.Header
	for _, batch := range s.chunkBatches(ctx, id, ids) {
		rows := s.fetchRows(ctx, id, batch)
		for j := range rows {
			if rows[j].Data == nil {
				continue
			}
			row := rows[j]
			c, err := chunk.Open(row.Data)
			if err != nil {
				return nil, nil, err
			}
			if header == nil {
				header = c.Header()
			} else if !sameColumns(header, c.Header()) {
				return nil, nil, errors.New("collection chunks have differing schemas")
			}
			feats, err := c.SearchAll()
			if err != nil {
				return nil, nil, err
			}
			set := ords[row.ID]
			for i, f := range feats {
				if !set[i] {
					continue
				}
				g := c.GeoJSON(f)
				if !inBounds(g, bbox) {
					continue
				}
				features = append(features, f)
			}
		}
	}
	return features, header, nil
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

const flatgeobufLimit = 100000

func parseBBox(r *http.Request) (*[4]float64, error) {
	value := r.URL.Query().Get("bbox")
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return nil, errors.New("bbox must be minx,miny,maxx,maxy")
	}
	var bbox [4]float64
	for i, part := range parts {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, errors.New("invalid bbox")
		}
		bbox[i] = n
	}
	return &bbox, nil
}

func parseLimit(r *http.Request, fallback int) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 1000000 {
		return 0, errors.New("invalid limit")
	}
	return n, nil
}

func parseOffset(r *http.Request) (int, error) {
	value := r.URL.Query().Get("offset")
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, errors.New("invalid offset")
	}
	return n, nil
}

func (s *Server) collectView(ctx context.Context, info store.CollectionInfo, bbox *[4]float64, filter cql.Expr, offset, limit int) ([]*flattypes.Feature, *flattypes.Header, error) {
	var features []*flattypes.Feature
	var header *flattypes.Header
	remaining := offset
	for _, member := range s.streamTargets(ctx, info) {
		if len(features) >= limit {
			break
		}
		collected, memberHeader, err := s.collectFeatures(ctx, member.Name, bbox, filter, remaining, limit-len(features), 0)
		if err != nil {
			return nil, nil, err
		}
		if header == nil {
			header = memberHeader
		}
		remaining = 0
		features = append(features, collected...)
	}
	return features, header, nil
}
