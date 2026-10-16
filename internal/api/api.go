package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

type Server struct {
	Store        *store.Store
	MaxChunks    int
	DefaultLimit int
}

type link struct {
	Href  string `json:"href"`
	Rel   string `json:"rel"`
	Type  string `json:"type,omitempty"`
	Title string `json:"title,omitempty"`
}

func New(st *store.Store) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("postfgb", "0.1.0"))
	s := &Server{Store: st, MaxChunks: 64, DefaultLimit: 1000}
	s.register(api)
	mux.HandleFunc("GET /collections/{id}/items", s.items)
	mux.HandleFunc("GET /collections/{id}/chunks/{chunkID}", s.chunkBytes)
	mux.HandleFunc("GET /collections/{id}/tiles/WebMercatorQuad/{z}/{x}/{y}", s.tile)
	return mux
}

func (s *Server) register(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "landing", Method: "GET", Path: "/", Summary: "Landing page"}, s.landing)
	huma.Register(api, huma.Operation{OperationID: "conformance", Method: "GET", Path: "/conformance", Summary: "Conformance"}, s.conformance)
	huma.Register(api, huma.Operation{OperationID: "collections", Method: "GET", Path: "/collections", Summary: "Collections"}, s.collections)
	huma.Register(api, huma.Operation{OperationID: "collection", Method: "GET", Path: "/collections/{id}", Summary: "Collection"}, s.collection)
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
			{Href: "/openapi.json", Rel: "service-desc", Type: "application/vnd.oai.openapi+json;version=3.1", Title: "API definition"},
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
		"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/geojson",
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
			{Href: "/collections/" + info.Name + "/tiles/WebMercatorQuad", Rel: "http://www.opengis.net/def/rel/ogc/1.0/tilesets-vector", Type: "application/json"},
		},
	}
	if info.MinX != nil && info.MinY != nil && info.MaxX != nil && info.MaxY != nil {
		body.Extent = &extent{Spatial: spatialExtent{
			BBox: [][]float64{{*info.MinX, *info.MinY, *info.MaxX, *info.MaxY}},
		}}
	}
	return body
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
	rows, err := s.Store.Chunks(ctx, id, bbox, s.MaxChunks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	out := bufio.NewWriter(w)
	defer out.Flush()
	out.WriteString(`{"type":"FeatureCollection","features":[`)
	count := 0
	for _, row := range rows {
		if count >= limit {
			break
		}
		c, err := chunk.Open(row.Data)
		if err != nil {
			continue
		}
		features, err := c.SearchOptional(bbox)
		if err != nil {
			continue
		}
		for _, f := range features {
			if count >= limit {
				break
			}
			body, err := json.Marshal(c.GeoJSON(f))
			if err != nil {
				continue
			}
			if count > 0 {
				out.WriteByte(',')
			}
			out.Write(body)
			count++
		}
	}
	out.WriteString(`],"numberReturned":`)
	out.WriteString(strconv.Itoa(count))
	out.WriteString("}")
}

func (s *Server) chunkBytes(w http.ResponseWriter, r *http.Request) {
	chunkID, err := strconv.ParseInt(r.PathValue("chunkID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid chunk id", http.StatusBadRequest)
		return
	}
	data, err := s.Store.ChunkByID(r.Context(), r.PathValue("id"), chunkID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "chunk not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/flatgeobuf")
	w.Write(data)
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
	if err != nil || n < 1 || n > 100000 {
		return 0, errors.New("invalid limit")
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
