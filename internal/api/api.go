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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/simplify"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

type Server struct {
	Store        *store.Store
	MaxChunks    int
	DefaultLimit int
}

type link struct {
	Href      string `json:"href"`
	Rel       string `json:"rel"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title,omitempty"`
	Templated bool   `json:"templated,omitempty"`
}

func New(st *store.Store) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("postfgb", "0.1.0"))
	s := &Server{Store: st, MaxChunks: 64, DefaultLimit: 1000}
	s.register(api)
	mux.HandleFunc("GET /collections/{id}/items", s.items)
	mux.HandleFunc("GET /collections/{id}/chunks/{chunkID}", s.chunkBytes)
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

type extent struct {
	Spatial spatialExtent `json:"spatial"`
}

type collection struct {
	ID     string  `json:"id"`
	Title  string  `json:"title,omitempty"`
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
		out.Body.Collections = append(out.Body.Collections, s.collectionBody(info))
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
	return &collectionOutput{Body: s.collectionBody(info)}, nil
}

func (s *Server) collectionBody(info store.CollectionInfo) collection {
	body := collection{
		ID:    info.Name,
		Title: info.Name,
		Links: []link{
			{Href: "/collections/" + info.Name, Rel: "self", Type: "application/json"},
			{Href: "/collections/" + info.Name + "/items", Rel: "items", Type: "application/geo+json"},
			{Href: "/collections/" + info.Name + "/tiles", Rel: "http://www.opengis.net/def/rel/ogc/1.0/tilesets-vector", Type: "application/json"},
		},
	}
	if info.MinX != nil && info.MinY != nil && info.MaxX != nil && info.MaxY != nil {
		body.Extent = &extent{Spatial: spatialExtent{
			BBox: [][]float64{{*info.MinX, *info.MinY, *info.MaxX, *info.MaxY}},
		}}
	}
	return body
}

var reservedParams = map[string]bool{
	"bbox": true, "limit": true, "offset": true, "q": true, "f": true,
	"filter": true, "filter-lang": true,
}

func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.Store.Collection(ctx, id); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bbox, err := parseBBox(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := parseLimit(r, s.DefaultLimit)
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
	query := r.URL.Query().Get("q")
	if r.URL.Query().Get("f") == "flatgeobuf" {
		s.writeFGB(ctx, w, id, bbox, query, filter, offset, limit)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	fw := newFeatureWriter(w, limit)
	if query != "" || filter != nil {
		s.streamMatches(ctx, fw, id, bbox, query, filter, offset)
	} else {
		s.streamBBox(ctx, fw, id, bbox, offset)
	}
	fw.close(pageLink(r, limit, offset, fw.count))
}

func (s *Server) streamBBox(ctx context.Context, fw *featureWriter, id string, bbox *[4]float64, offset int) {
	metas, err := s.Store.ChunkMetas(ctx, id, bbox, s.MaxChunks)
	if err != nil {
		slog.Error("chunk metas query failed", "collection", id, "error", err)
		return
	}
	skipped := 0
	for _, meta := range metas {
		if fw.count >= fw.limit {
			return
		}
		if bbox == nil && skipped+meta.FeatureCount <= offset {
			skipped += meta.FeatureCount
			continue
		}
		row, err := s.Store.ChunkByID(ctx, id, meta.ID)
		if err != nil {
			slog.Error("chunk load failed", "chunk", meta.ID, "error", err)
			continue
		}
		c, err := chunk.Open(row.Data)
		if err != nil {
			slog.Error("chunk open failed", "chunk", meta.ID, "error", err)
			continue
		}
		features, err := c.SearchOptional(bbox)
		if err != nil {
			slog.Error("chunk search failed", "chunk", meta.ID, "error", err)
			continue
		}
		for _, f := range features {
			if skipped < offset {
				skipped++
				continue
			}
			if !fw.add(c.GeoJSON(f)) {
				return
			}
		}
	}
}

func (s *Server) streamMatches(ctx context.Context, fw *featureWriter, id string, bbox *[4]float64, query string, filter cql.Expr, offset int) {
	matches, err := s.Store.Matches(ctx, id, bbox, query, filter, fw.limit, offset)
	if err != nil {
		slog.Error("matches query failed", "collection", id, "error", err)
		return
	}
	byChunk := map[int64]map[int]bool{}
	var ids []int64
	for _, m := range matches {
		ords := byChunk[m.ChunkID]
		if ords == nil {
			if len(ids) >= s.MaxChunks {
				break
			}
			ords = map[int]bool{}
			byChunk[m.ChunkID] = ords
			ids = append(ids, m.ChunkID)
		}
		ords[m.Ord] = true
	}
	rows, err := s.Store.ChunksByIDs(ctx, ids)
	if err != nil {
		slog.Error("chunks by ids failed", "collection", id, "error", err)
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
		ords := byChunk[row.ID]
		for i, f := range features {
			if !ords[i] {
				continue
			}
			g := c.GeoJSON(f)
			if !inBounds(g, bbox) {
				continue
			}
			if !fw.add(g) {
				return
			}
		}
	}
}

type featureWriter struct {
	out   *bufio.Writer
	count int
	limit int
	first bool
}

func newFeatureWriter(w http.ResponseWriter, limit int) *featureWriter {
	fw := &featureWriter{out: bufio.NewWriter(w), limit: limit, first: true}
	fw.out.WriteString(`{"type":"FeatureCollection","features":[`)
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

func inBounds(f *chunk.Feature, bbox *[4]float64) bool {
	if bbox == nil {
		return true
	}
	geom := toOrb(f.Geometry)
	if geom == nil {
		return false
	}
	bound := geom.Bound()
	return bound.Max[0] >= bbox[0] && bound.Min[0] <= bbox[2] &&
		bound.Max[1] >= bbox[1] && bound.Min[1] <= bbox[3]
}

func (s *Server) writeFGB(ctx context.Context, w http.ResponseWriter, id string, bbox *[4]float64, query string, filter cql.Expr, offset, limit int) {
	features, header, err := s.collectFeatures(ctx, id, bbox, query, filter, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(features) == 0 {
		w.Header().Set("Content-Type", "application/geo+json")
		fw := newFeatureWriter(w, limit)
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

func (s *Server) collectFeatures(ctx context.Context, id string, bbox *[4]float64, query string, filter cql.Expr, offset, limit int) ([]*flattypes.Feature, *flattypes.Header, error) {
	if query != "" || filter != nil {
		return s.collectMatches(ctx, id, bbox, query, filter, offset, limit)
	}
	metas, err := s.Store.ChunkMetas(ctx, id, bbox, s.MaxChunks)
	if err != nil {
		return nil, nil, err
	}
	var features []*flattypes.Feature
	var header *flattypes.Header
	skipped := 0
	for _, meta := range metas {
		if len(features) >= limit {
			break
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
		feats, err := c.SearchOptional(bbox)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range feats {
			if skipped < offset {
				skipped++
				continue
			}
			if len(features) >= limit {
				break
			}
			features = append(features, f)
		}
	}
	return features, header, nil
}

func (s *Server) collectMatches(ctx context.Context, id string, bbox *[4]float64, query string, filter cql.Expr, offset, limit int) ([]*flattypes.Feature, *flattypes.Header, error) {
	matches, err := s.Store.Matches(ctx, id, bbox, query, filter, limit, offset)
	if err != nil {
		return nil, nil, err
	}
	byChunk := map[int64]map[int]bool{}
	var ids []int64
	for _, m := range matches {
		ords := byChunk[m.ChunkID]
		if ords == nil {
			if len(ids) >= s.MaxChunks {
				break
			}
			ords = map[int]bool{}
			byChunk[m.ChunkID] = ords
			ids = append(ids, m.ChunkID)
		}
		ords[m.Ord] = true
	}
	rows, err := s.Store.ChunksByIDs(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	var features []*flattypes.Feature
	var header *flattypes.Header
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
		ords := byChunk[row.ID]
		for i, f := range feats {
			if !ords[i] {
				continue
			}
			if !inBounds(c.GeoJSON(f), bbox) {
				continue
			}
			features = append(features, f)
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

func (s *Server) chunkBytes(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "application/flatgeobuf")
	w.Write(row.Data)
}

func (s *Server) tile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	z, errZ := strconv.ParseUint(r.PathValue("z"), 10, 32)
	x, errX := strconv.ParseUint(r.PathValue("x"), 10, 32)
	y, errY := strconv.ParseUint(strings.TrimSuffix(r.PathValue("y"), ".mvt"), 10, 32)
	if errZ != nil || errX != nil || errY != nil {
		http.Error(w, "invalid tile coordinates", http.StatusBadRequest)
		return
	}
	tile := maptile.New(uint32(x), uint32(y), maptile.Zoom(z))
	bound := tile.Bound()
	bbox := [4]float64{bound.Min[0], bound.Min[1], bound.Max[0], bound.Max[1]}
	rows, err := s.Store.Chunks(ctx, id, &bbox, s.MaxChunks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	collection := orbgeojson.NewFeatureCollection()
	for _, row := range rows {
		c, err := chunk.Open(row.Data)
		if err != nil {
			continue
		}
		raw, err := c.Search(bbox[0], bbox[1], bbox[2], bbox[3])
		if err != nil {
			continue
		}
		for _, f := range raw {
			g := c.GeoJSON(f)
			geom := toOrb(g.Geometry)
			if geom == nil {
				continue
			}
			feature := orbgeojson.NewFeature(geom)
			feature.Properties = g.Properties
			collection.Append(feature)
		}
	}
	layer := mvt.NewLayer(id, collection)
	layer.ProjectToTile(tile)
	layer.Clip(mvt.MapboxGLDefaultExtentBound)
	layer.Simplify(simplify.DouglasPeucker(1.0))
	layer.RemoveEmpty(1.0, 2.0)
	data, err := mvt.MarshalGzipped(mvt.Layers{layer})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
	w.Write(data)
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

func toOrb(g *chunk.Geometry) orb.Geometry {
	if g == nil {
		return nil
	}
	switch g.Type {
	case "Point":
		pts, ok := g.Coordinates.([]float64)
		if !ok || len(pts) < 2 {
			return nil
		}
		return orb.Point{pts[0], pts[1]}
	case "LineString":
		pts, ok := g.Coordinates.([][]float64)
		if !ok {
			return nil
		}
		return orbLine(pts)
	case "Polygon":
		rs, ok := g.Coordinates.([][][]float64)
		if !ok {
			return nil
		}
		return orbPoly(rs)
	case "MultiPoint":
		pts, ok := g.Coordinates.([][]float64)
		if !ok {
			return nil
		}
		mp := make(orb.MultiPoint, 0, len(pts))
		for _, p := range pts {
			if len(p) < 2 {
				continue
			}
			mp = append(mp, orb.Point{p[0], p[1]})
		}
		return mp
	case "MultiLineString":
		ls, ok := g.Coordinates.([][][]float64)
		if !ok {
			return nil
		}
		ml := make(orb.MultiLineString, 0, len(ls))
		for _, l := range ls {
			ml = append(ml, orbLine(l))
		}
		return ml
	case "MultiPolygon":
		ps, ok := g.Coordinates.([][][][]float64)
		if !ok {
			return nil
		}
		mp := make(orb.MultiPolygon, 0, len(ps))
		for _, p := range ps {
			mp = append(mp, orbPoly(p))
		}
		return mp
	case "GeometryCollection":
		var parts []orb.Geometry
		for i := range g.Geometries {
			if part := toOrb(&g.Geometries[i]); part != nil {
				parts = append(parts, part)
			}
		}
		return orb.Collection(parts)
	}
	return nil
}

func orbLine(pts [][]float64) orb.LineString {
	line := make(orb.LineString, 0, len(pts))
	for _, p := range pts {
		if len(p) < 2 {
			continue
		}
		line = append(line, orb.Point{p[0], p[1]})
	}
	return line
}

func orbPoly(rs [][][]float64) orb.Polygon {
	poly := make(orb.Polygon, 0, len(rs))
	for _, r := range rs {
		poly = append(poly, orb.Ring(orbLine(r)))
	}
	return poly
}
