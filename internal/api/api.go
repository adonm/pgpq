package api

import (
	"bufio"
	"context"
	"crypto/subtle"
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
	"github.com/paulmach/orb/encoding/mvt"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
	"github.com/adonm/postfgb/internal/tiles"
)

type Server struct {
	Store        *store.Store
	MaxChunks    int
	DefaultLimit int
	AdminToken   string
	RustgisCLI   string
	OGR2OGR      string
	sweepMu      sync.Mutex
	lastSweep    time.Time
}

type Config struct {
	Store      *store.Store
	AdminToken string
	RustgisCLI string
	OGR2OGR    string
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
	s := &Server{
		Store: cfg.Store, MaxChunks: 64, DefaultLimit: 50000,
		AdminToken: cfg.AdminToken, RustgisCLI: cfg.RustgisCLI, OGR2OGR: cfg.OGR2OGR,
	}
	s.register(api)
	mux.HandleFunc("GET /collections/{id}/items", s.items)
	mux.HandleFunc("GET /collections/{id}/items/{featureID}", s.feature)
	mux.HandleFunc("PUT /collections/{id}/data", s.ingestData)
	mux.HandleFunc("PUT /collections/{id}/view", s.putView)
	mux.HandleFunc("DELETE /collections/{id}", s.deleteCollection)
	mux.HandleFunc("GET /collections/{id}/chunks/{chunkID}", s.chunkBytes)
	mux.HandleFunc("GET /collections/{id}/export", s.export)
	mux.HandleFunc("GET /collections/{id}/tiles/WebMercatorQuad/{z}/{x}/{y}", s.tile)
	mux.HandleFunc("GET /api", func(w http.ResponseWriter, r *http.Request) {
		body, err := api.OpenAPI().Downgrade()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.0")
		w.Write(body)
	})
	return gzipMiddleware(mux)
}

func (s *Server) register(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "landing", Method: "GET", Path: "/", Summary: "Landing page"}, s.landing)
	huma.Register(api, huma.Operation{OperationID: "conformance", Method: "GET", Path: "/conformance", Summary: "Conformance"}, s.conformance)
	huma.Register(api, huma.Operation{OperationID: "collections", Method: "GET", Path: "/collections", Summary: "Collections"}, s.collections)
	huma.Register(api, huma.Operation{OperationID: "collection", Method: "GET", Path: "/collections/{id}", Summary: "Collection"}, s.collection)
	huma.Register(api, huma.Operation{OperationID: "queryables", Method: "GET", Path: "/collections/{id}/queryables", Summary: "Queryables"}, s.queryables)
	huma.Register(api, huma.Operation{OperationID: "collectionTilesets", Method: "GET", Path: "/collections/{id}/tiles", Summary: "Collection tilesets"}, s.tilesets)
	huma.Register(api, huma.Operation{OperationID: "manifest", Method: "GET", Path: "/collections/{id}/manifest", Summary: "Chunk manifest"}, s.manifest)
	huma.Register(api, huma.Operation{OperationID: "tileMatrixSets", Method: "GET", Path: "/tileMatrixSets", Summary: "Tile matrix sets"}, s.tileMatrixSets)
	huma.Register(api, huma.Operation{OperationID: "tileMatrixSet", Method: "GET", Path: "/tileMatrixSets/{tileMatrixSetId}", Summary: "Tile matrix set"}, s.tileMatrixSet)
}

type landing struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Links       []link `json:"links"`
}

type landingOutput struct {
	Body landing
}

func (s *Server) landing(context.Context, *struct{}) (*landingOutput, error) {
	return &landingOutput{Body: landing{
		Title:       "postfgb",
		Description: "OGC API Features over FlatGeobuf chunks stored in PostgreSQL",
		Links: []link{
			{Href: "/", Rel: "self", Type: "application/json", Title: "This document"},
			{Href: "/api", Rel: "service-desc", Type: "application/vnd.oai.openapi+json;version=3.0", Title: "API definition"},
			{Href: "/conformance", Rel: "conformance", Type: "application/json", Title: "Conformance classes"},
			{Href: "/collections", Rel: "data", Type: "application/json", Title: "Collections"},
		},
	}}, nil
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
		"http://www.opengis.net/spec/cql2/1.0/conf/cql2-json",
		"http://www.opengis.net/spec/cql2/1.0/conf/basic-cql2",
		"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/core",
		"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/oas30",
		"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/tileset",
		"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/tilesets-list",
		"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/geodata-tilesets",
		"http://www.opengis.net/spec/ogcapi-tiles-1/1.0/conf/mvt",
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
	ID     string  `json:"id"`
	Title  string  `json:"title,omitempty"`
	Kind   string  `json:"kind,omitempty"`
	Extent *extent `json:"extent,omitempty"`
	Links  []link  `json:"links"`
}

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
	body := collection{
		ID:    info.Name,
		Title: info.Name,
		Kind:  info.Kind,
		Links: []link{
			{Href: "/collections/" + info.Name, Rel: "self", Type: "application/json"},
			{Href: "/collections/" + info.Name + "/items", Rel: "items", Type: "application/geo+json"},
			{Href: "/collections/" + info.Name + "/items?f=flatgeobuf", Rel: "alternate", Type: "application/flatgeobuf", Title: "Items (FlatGeobuf)"},
			{Href: "/collections/" + info.Name + "/tiles", Rel: "http://www.opengis.net/def/rel/ogc/1.0/tilesets-vector", Type: "application/json"},
		},
	}
	body.Extent = s.collectionExtent(ctx, info)
	return body
}

func (s *Server) collectionExtent(ctx context.Context, info store.CollectionInfo) *extent {
	if info.Kind != "view" {
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
	members, err := parseMembers(info)
	if err != nil {
		return nil
	}
	var minX, minY, maxX, maxY float64
	var timeMin, timeMax *time.Time
	spatial := false
	for _, member := range members {
		mi, err := s.Store.Collection(ctx, member.Collection)
		if err != nil {
			continue
		}
		if mi.MinX != nil && mi.MinY != nil && mi.MaxX != nil && mi.MaxY != nil {
			if !spatial {
				minX, minY, maxX, maxY = *mi.MinX, *mi.MinY, *mi.MaxX, *mi.MaxY
				spatial = true
			} else {
				minX = min(minX, *mi.MinX)
				minY = min(minY, *mi.MinY)
				maxX = max(maxX, *mi.MaxX)
				maxY = max(maxY, *mi.MaxY)
			}
		}
		if mi.TimeMin != nil && mi.TimeMax != nil {
			if timeMin == nil || mi.TimeMin.Before(*timeMin) {
				timeMin = mi.TimeMin
			}
			if timeMax == nil || mi.TimeMax.After(*timeMax) {
				timeMax = mi.TimeMax
			}
		}
	}
	if !spatial && timeMin == nil {
		return nil
	}
	out := &extent{}
	if spatial {
		out.Spatial = spatialExtent{BBox: [][]float64{{minX, minY, maxX, maxY}}}
	}
	if timeMin != nil && timeMax != nil {
		out.Temporal = timeInterval(*timeMin, *timeMax)
	}
	return out
}

func timeInterval(minT, maxT time.Time) *temporalExtent {
	return &temporalExtent{Interval: [][]string{{minT.UTC().Format(time.RFC3339), maxT.UTC().Format(time.RFC3339)}}}
}

var reservedParams = map[string]bool{
	"bbox": true, "limit": true, "offset": true, "q": true, "f": true,
	"filter": true, "filter-lang": true, "format": true,
	"overview": true, "numberMatched": true,
	"datetime": true, "sortby": true, "crs": true, "bbox-crs": true,
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
	bbox, err := parseBBox(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bbox, err = s.inputBBox(r, bbox)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	crsFn, contentCRS, err := s.outputCRS(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	overview, err := parseOverview(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flatgeobuf := r.URL.Query().Get("f") == "flatgeobuf" ||
		strings.Contains(r.Header.Get("Accept"), "application/flatgeobuf")
	defaultLimit := s.DefaultLimit
	if flatgeobuf {
		defaultLimit = 1000000
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
	filter, err := requestFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dt, err := datetimeFilter(r, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter = cql.And(filter, dt)
	order, err := s.sortOrder(ctx, r, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := r.URL.Query().Get("q")
	var matched *int64
	if r.URL.Query().Get("numberMatched") == "true" {
		count, ok, err := s.countMatches(ctx, info, bbox, query, filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			matched = &count
		}
	}
	if info.Kind == "view" {
		if flatgeobuf {
			http.Error(w, "flatgeobuf export of views is not supported", http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/geo+json")
		if contentCRS != "" {
			w.Header().Set("Content-Crs", contentCRS)
		}
		fw := newFeatureWriter(w, limit, matched)
		var sink featureSink = fw
		if crsFn != nil {
			sink = &crsWriter{base: sink, fn: crsFn}
		}
		s.streamView(ctx, sink, info, bbox, query, filter, offset, overview)
		fw.close(pageLink(r, limit, offset, fw.count))
		return
	}
	if flatgeobuf {
		s.writeFGB(ctx, w, id, bbox, query, filter, order, offset, limit, 0)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	if contentCRS != "" {
		w.Header().Set("Content-Crs", contentCRS)
	}
	fw := newFeatureWriter(w, limit, matched)
	var sink featureSink = fw
	if crsFn != nil {
		sink = &crsWriter{base: sink, fn: crsFn}
	}
	switch {
	case order != nil:
		s.streamSorted(ctx, sink, info, bbox, query, filter, order, offset)
	case query != "" || filter != nil:
		s.streamMatches(ctx, sink, info, bbox, query, filter, offset)
	default:
		tier := tierForBounds(bbox)
		if overview != nil {
			tier = *overview
		}
		s.streamBBox(ctx, sink, info, bbox, offset, tier)
	}
	fw.close(pageLink(r, limit, offset, fw.count))
}

func (s *Server) streamBBox(ctx context.Context, sink featureSink, info store.CollectionInfo, bbox *[4]float64, offset, tier int) int {
	after := int64(0)
	fallback := tier > 0
	skipped := 0
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
		for _, meta := range metas {
			if sink.full() {
				return skipped
			}
			if bbox == nil && skipped+meta.FeatureCount <= offset {
				skipped += meta.FeatureCount
				continue
			}
			row, err := s.Store.ChunkByID(ctx, info.Name, meta.ID)
			if err != nil {
				slog.Error("chunk load failed", "chunk", meta.ID, "error", err)
				continue
			}
			c, err := chunk.Open(row.Data)
			if err != nil {
				slog.Error("chunk open failed", "chunk", meta.ID, "error", err)
				continue
			}
			features, err := c.SearchAll()
			if err != nil {
				slog.Error("chunk search failed", "chunk", meta.ID, "error", err)
				continue
			}
			for i, f := range features {
				g := c.GeoJSON(f)
				if !inBounds(g, bbox) {
					continue
				}
				if skipped < offset {
					skipped++
					continue
				}
				g.ID = featureID(info, meta.ID, i, g.Properties)
				if !sink.add(g) {
					return skipped
				}
			}
		}
		if len(metas) < s.MaxChunks {
			return skipped
		}
		after = metas[len(metas)-1].ID
	}
}

func (s *Server) streamMatches(ctx context.Context, sink featureSink, info store.CollectionInfo, bbox *[4]float64, query string, filter cql.Expr, offset int) {
	matches, err := s.Store.Matches(ctx, info.Name, bbox, query, filter, sink.remaining(), offset)
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
	for start := 0; start < len(ids); start += s.MaxChunks {
		end := start + s.MaxChunks
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := s.Store.ChunksByIDs(ctx, info.Name, ids[start:end])
		if err != nil {
			slog.Error("chunks by ids failed", "collection", info.Name, "error", err)
			return
		}
		for _, row := range rows {
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
	var parsed cql.Expr
	var err error
	switch r.URL.Query().Get("filter-lang") {
	case "", "cql2-text":
		parsed, err = cql.Parse(text)
	case "cql2-json":
		parsed, err = cql.ParseJSON([]byte(text))
	default:
		return nil, errors.New("unsupported filter-lang " + r.URL.Query().Get("filter-lang"))
	}
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

func (s *Server) countMatches(ctx context.Context, info store.CollectionInfo, bbox *[4]float64, query string, filter cql.Expr) (int64, bool, error) {
	if info.Kind == "view" {
		if bbox != nil || query != "" || filter != nil {
			return 0, false, nil
		}
		members, err := parseMembers(info)
		if err != nil {
			return 0, false, err
		}
		var total int64
		for _, member := range members {
			if member.Filter != "" {
				return 0, false, nil
			}
			mi, err := s.Store.Collection(ctx, member.Collection)
			if err != nil {
				return 0, false, err
			}
			total += mi.FeatureCount
		}
		return total, true, nil
	}
	if query != "" || filter != nil {
		count, err := s.Store.CountMatches(ctx, info.Name, bbox, query, filter)
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
	geom := tiles.ToOrb(f.Geometry)
	if geom == nil {
		return false
	}
	bound := geom.Bound()
	return bound.Max[0] >= bbox[0] && bound.Min[0] <= bbox[2] &&
		bound.Max[1] >= bbox[1] && bound.Min[1] <= bbox[3]
}

func (s *Server) writeFGB(ctx context.Context, w http.ResponseWriter, id string, bbox *[4]float64, query string, filter cql.Expr, order []string, offset, limit, tier int) {
	features, header, err := s.collectFeatures(ctx, id, bbox, query, filter, order, offset, limit, tier)
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
	w.Write(data)
}

func (s *Server) collectFeatures(ctx context.Context, id string, bbox *[4]float64, query string, filter cql.Expr, order []string, offset, limit, tier int) ([]*flattypes.Feature, *flattypes.Header, error) {
	if len(order) > 0 {
		return s.collectSorted(ctx, id, bbox, query, filter, order, offset, limit)
	}
	if query != "" || filter != nil {
		return s.collectMatches(ctx, id, bbox, query, filter, offset, limit)
	}
	var features []*flattypes.Feature
	var header *flattypes.Header
	skipped := 0
	after := int64(0)
	fallback := tier > 0
	for {
		metas, err := s.Store.ChunkMetas(ctx, id, bbox, s.MaxChunks, tier, after)
		if err != nil {
			return nil, nil, err
		}
		if len(metas) == 0 {
			if fallback && after == 0 {
				tier = 0
				fallback = false
				continue
			}
			break
		}
		fallback = false
		for _, meta := range metas {
			if len(features) >= limit {
				return features, header, nil
			}
			if bbox == nil && skipped+meta.FeatureCount <= offset {
				skipped += meta.FeatureCount
				continue
			}
			row, err := s.Store.ChunkByID(ctx, id, meta.ID)
			if err != nil {
				return nil, nil, err
			}
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
			for _, f := range feats {
				if !inBounds(c.GeoJSON(f), bbox) {
					continue
				}
				if skipped < offset {
					skipped++
					continue
				}
				if len(features) >= limit {
					return features, header, nil
				}
				features = append(features, f)
			}
		}
		if len(metas) < s.MaxChunks {
			break
		}
		after = metas[len(metas)-1].ID
	}
	return features, header, nil
}

func (s *Server) collectMatches(ctx context.Context, id string, bbox *[4]float64, query string, filter cql.Expr, offset, limit int) ([]*flattypes.Feature, *flattypes.Header, error) {
	matches, err := s.Store.Matches(ctx, id, bbox, query, filter, limit, offset)
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
	for start := 0; start < len(ids); start += s.MaxChunks {
		end := start + s.MaxChunks
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := s.Store.ChunksByIDs(ctx, id, ids[start:end])
		if err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
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
				if !inBounds(c.GeoJSON(f), bbox) {
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

func (s *Server) adminOK(auth string) bool {
	if s.AdminToken == "" {
		return true
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return token != auth && subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminToken)) == 1
}

func (s *Server) chunkBytes(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	chunkID, err := strconv.ParseInt(r.PathValue("chunkID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid chunk id", http.StatusBadRequest)
		return
	}
	row, err := s.Store.ChunkByID(r.Context(), r.PathValue("id"), chunkID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "chunk not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if row.Checksum != nil {
		etag := `"` + *row.Checksum + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	raw, err := chunk.Decompress(row.Data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "application/flatgeobuf")
	w.Write(raw)
}

func (s *Server) tile(w http.ResponseWriter, r *http.Request) {
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
	z, errZ := strconv.ParseUint(r.PathValue("z"), 10, 32)
	x, errX := strconv.ParseUint(r.PathValue("x"), 10, 32)
	y, errY := strconv.ParseUint(strings.TrimSuffix(r.PathValue("y"), ".mvt"), 10, 32)
	if errZ != nil || errX != nil || errY != nil {
		http.Error(w, "invalid tile coordinates", http.StatusBadRequest)
		return
	}
	filter, err := requestFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	overview, err := parseOverview(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dt, err := datetimeFilter(r, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter = cql.And(filter, dt)
	query := r.URL.Query().Get("q")
	tile := maptile.New(uint32(x), uint32(y), maptile.Zoom(z))
	bound := tile.Bound()
	bbox := [4]float64{bound.Min[0], bound.Min[1], bound.Max[0], bound.Max[1]}
	filtered := query != "" || filter != nil
	tier := 0
	if !filtered {
		tier = tierForZoom(int(z))
		if overview != nil {
			tier = *overview
		}
	}
	version, err := s.cacheVersion(ctx, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filterKey := filterKeyOf(r)
	etag := tileETag(version, tier, int(z), int(x), int(y), filterKey)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.maybeSweep()
	if cached, cachedEtag, ok, err := s.Store.TileCacheGet(ctx, info.Name, version, tier, int(z), int(x), int(y), filterKey); err != nil {
		slog.Error("tile cache get failed", "collection", info.Name, "error", err)
	} else if ok {
		w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
		w.Header().Set("ETag", cachedEtag)
		w.Header().Set("Cache-Control", "public, max-age=600")
		w.Write(cached)
		return
	}
	var collection *orbgeojson.FeatureCollection
	switch {
	case info.Kind == "view":
		collection, err = s.viewTile(ctx, info, bbox, query, filter, tier)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case query == "" && filter == nil:
		rows, err := s.Store.Chunks(ctx, id, &bbox, s.MaxChunks, tier)
		if err == nil && len(rows) == 0 && tier > 0 {
			rows, err = s.Store.Chunks(ctx, id, &bbox, s.MaxChunks, 0)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		collection = orbgeojson.NewFeatureCollection()
		for _, row := range rows {
			c, err := chunk.Open(row.Data)
			if err != nil {
				continue
			}
			raw, err := c.Search(bbox[0], bbox[1], bbox[2], bbox[3])
			if err != nil {
				continue
			}
			appendFeatures(collection, c, raw)
		}
	default:
		collection, err = s.tileMatches(ctx, id, bbox, query, filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	data, empty, err := tiles.Bytes(id, collection, tile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if empty {
		data, err = mvt.MarshalGzipped(mvt.Layers{mvt.NewLayer(id, orbgeojson.NewFeatureCollection())})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Write(data)
	if len(data) <= maxTileCacheBytes {
		if err := s.Store.TileCachePut(ctx, info.Name, version, tier, int(z), int(x), int(y), filterKey, etag, data); err != nil {
			slog.Error("tile cache put failed", "collection", info.Name, "error", err)
		}
	}
}

func appendFeatures(fc *orbgeojson.FeatureCollection, c *chunk.Chunk, feats []*flattypes.Feature) {
	for _, f := range feats {
		g := c.GeoJSON(f)
		geom := tiles.ToOrb(g.Geometry)
		if geom == nil {
			continue
		}
		feature := orbgeojson.NewFeature(geom)
		feature.Properties = g.Properties
		fc.Append(feature)
	}
}

func (s *Server) tileMatches(ctx context.Context, id string, bbox [4]float64, query string, filter cql.Expr) (*orbgeojson.FeatureCollection, error) {
	matches, err := s.Store.Matches(ctx, id, &bbox, query, filter, s.DefaultLimit, 0)
	if err != nil {
		return nil, err
	}
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
	fc := orbgeojson.NewFeatureCollection()
	for start := 0; start < len(ids); start += s.MaxChunks {
		end := start + s.MaxChunks
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := s.Store.ChunksByIDs(ctx, id, ids[start:end])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			c, err := chunk.Open(row.Data)
			if err != nil {
				continue
			}
			feats, err := c.SearchAll()
			if err != nil {
				continue
			}
			set := ords[row.ID]
			for i, f := range feats {
				if !set[i] {
					continue
				}
				g := c.GeoJSON(f)
				if !inBounds(g, &bbox) {
					continue
				}
				geom := tiles.ToOrb(g.Geometry)
				if geom == nil {
					continue
				}
				feature := orbgeojson.NewFeature(geom)
				feature.Properties = g.Properties
				fc.Append(feature)
			}
		}
	}
	return fc, nil
}

func tierForZoom(z int) int {
	switch {
	case z <= 2:
		return 2
	case z <= 4:
		return 1
	default:
		return 0
	}
}

func tierForBounds(bbox *[4]float64) int {
	if bbox == nil {
		return 2
	}
	area := (bbox[2] - bbox[0]) * (bbox[3] - bbox[1])
	switch {
	case area >= 400:
		return 2
	case area >= 20:
		return 1
	default:
		return 0
	}
}

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
