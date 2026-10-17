package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/geo"
	"github.com/adonm/postfgb/internal/store"
	"github.com/adonm/postfgb/internal/tiles"
)

const (
	stacVersion  = "1.0.0"
	stacMaxLimit = 10000
)

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	return scheme + "://" + r.Host
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}

type stacWriter struct {
	out   *bufio.Writer
	base  string
	count int
	limit int
	first bool
}

func newStacWriter(w http.ResponseWriter, matched *int64, base string, limit int) *stacWriter {
	sw := &stacWriter{out: bufio.NewWriter(w), base: base, limit: limit, first: true}
	sw.out.WriteString(`{"type":"FeatureCollection","stac_version":"` + stacVersion + `"`)
	if matched != nil {
		sw.out.WriteString(`,"numberMatched":`)
		sw.out.WriteString(strconv.FormatInt(*matched, 10))
	}
	sw.out.WriteString(`,"features":[`)
	return sw
}

func (sw *stacWriter) add(g *chunk.Feature, info store.CollectionInfo, member viewMember) bool {
	if sw.count >= sw.limit {
		return false
	}
	props := member.transformProps(g.Properties)
	if props == nil {
		props = map[string]any{}
	}
	if info.TimeColumn != nil && *info.TimeColumn != "" {
		if value, ok := props[*info.TimeColumn]; ok {
			props["datetime"] = value
		} else {
			props["datetime"] = nil
		}
	} else {
		props["datetime"] = nil
	}
	href := sw.base + "/collections/" + info.Name + "/items/" + fmt.Sprint(g.ID)
	item := map[string]any{
		"type":         "Feature",
		"stac_version": stacVersion,
		"id":           fmt.Sprint(g.ID),
		"collection":   info.Name,
		"geometry":     g.Geometry,
		"properties":   props,
		"links": []map[string]any{
			{"rel": "self", "href": href, "type": "application/geo+json"},
			{"rel": "collection", "href": sw.base + "/stac/collections/" + info.Name, "type": "application/json"},
		},
		"assets": map[string]any{
			"data": map[string]any{"href": href, "type": "application/geo+json", "roles": []string{"data"}},
		},
	}
	if bound := featureBounds(g); bound != nil {
		item["bbox"] = bound
	}
	body, err := json.Marshal(item)
	if err != nil {
		return true
	}
	if !sw.first {
		sw.out.WriteByte(',')
	}
	sw.out.Write(body)
	sw.first = false
	sw.count++
	return sw.count < sw.limit
}

func (sw *stacWriter) close(next string) {
	sw.out.WriteString(`],"numberReturned":`)
	sw.out.WriteString(strconv.Itoa(sw.count))
	if next != "" {
		body, err := json.Marshal(next)
		if err == nil {
			sw.out.WriteString(`,"links":[{"rel":"next","href":`)
			sw.out.Write(body)
			sw.out.WriteString(`,"type":"application/geo+json"}]`)
		}
	}
	sw.out.WriteString("}")
	sw.out.Flush()
}

func featureBounds(g *chunk.Feature) []float64 {
	geom := tiles.ToOrb(g.Geometry)
	if geom == nil {
		return nil
	}
	bound := geom.Bound()
	return []float64{bound.Min[0], bound.Min[1], bound.Max[0], bound.Max[1]}
}

type stacMember struct {
	w      *stacWriter
	info   store.CollectionInfo
	member viewMember
}

func (m *stacMember) add(g *chunk.Feature) bool {
	return m.w.add(g, m.info, m.member)
}

func (m *stacMember) full() bool {
	return m.w.count >= m.w.limit
}

func (m *stacMember) remaining() int {
	return m.w.limit - m.w.count
}

func (s *Server) stacSinkFor(sw *stacWriter, shapes []geo.Shape) func(store.CollectionInfo, viewMember) featureSink {
	return func(info store.CollectionInfo, member viewMember) featureSink {
		var sink featureSink = &stacMember{w: sw, info: info, member: member}
		if len(shapes) > 0 {
			sink = &spatialWriter{base: sink, shapes: shapes}
		}
		return sink
	}
}

func parseStacLimit(r *http.Request, fallback int) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > stacMaxLimit {
		return 0, errors.New("limit must be between 1 and " + strconv.Itoa(stacMaxLimit))
	}
	return n, nil
}

func (s *Server) stacCatalog(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	writeJSON(w, map[string]any{
		"type":         "Catalog",
		"stac_version": stacVersion,
		"id":           "postfgb",
		"title":        "postfgb",
		"description":  "OGC API Features and OGC Tiles over FlatGeobuf chunks stored in PostgreSQL",
		"links": []map[string]any{
			{"rel": "self", "href": base + "/stac", "type": "application/json"},
			{"rel": "root", "href": base + "/stac", "type": "application/json"},
			{"rel": "data", "href": base + "/stac/collections", "type": "application/json"},
			{"rel": "conformance", "href": base + "/conformance", "type": "application/json"},
		},
	})
}

func (s *Server) stacCollections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	infos, err := s.Store.Collections(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := baseURL(r)
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, s.stacCollectionBody(ctx, info, base))
	}
	writeJSON(w, map[string]any{
		"collections": out,
		"links": []map[string]any{
			{"rel": "self", "href": base + "/stac/collections", "type": "application/json"},
			{"rel": "root", "href": base + "/stac", "type": "application/json"},
		},
	})
}

func (s *Server) stacCollection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info, err := s.Store.Collection(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.stacCollectionBody(ctx, info, baseURL(r)))
}

func (s *Server) stacCollectionBody(ctx context.Context, info store.CollectionInfo, base string) map[string]any {
	out := map[string]any{}
	if len(info.Stac) > 0 {
		var stored map[string]any
		if json.Unmarshal(info.Stac, &stored) == nil {
			out = stored
		}
	}
	out["type"] = "Collection"
	out["stac_version"] = stacVersion
	out["id"] = info.Name
	title := info.Name
	if info.Title != nil && *info.Title != "" {
		title = *info.Title
	}
	out["title"] = title
	if info.Description != nil {
		out["description"] = *info.Description
	} else if _, ok := out["description"]; !ok {
		out["description"] = info.Name
	}
	if info.License != nil && *info.License != "" {
		out["license"] = *info.License
	} else if _, ok := out["license"]; !ok {
		out["license"] = "proprietary"
	}
	if len(info.Keywords) > 0 {
		out["keywords"] = info.Keywords
	}
	spatial := [][]float64{{-180, -90, 180, 90}}
	interval := [][]any{{nil, nil}}
	if ext := s.collectionExtent(ctx, info); ext != nil {
		if len(ext.Spatial.BBox) > 0 {
			spatial = ext.Spatial.BBox
		}
		if ext.Temporal != nil && len(ext.Temporal.Interval) == 1 {
			pair := ext.Temporal.Interval[0]
			interval = [][]any{{pair[0], pair[1]}}
		}
	}
	out["extent"] = map[string]any{
		"spatial":  map[string]any{"bbox": spatial},
		"temporal": map[string]any{"interval": interval},
	}
	out["links"] = []map[string]any{
		{"rel": "self", "href": base + "/stac/collections/" + info.Name, "type": "application/json"},
		{"rel": "root", "href": base + "/stac", "type": "application/json"},
		{"rel": "parent", "href": base + "/stac/collections", "type": "application/json"},
		{"rel": "items", "href": base + "/stac/collections/" + info.Name + "/items", "type": "application/geo+json"},
	}
	return out
}

func (s *Server) stacItems(w http.ResponseWriter, r *http.Request) {
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
	limit, err := parseStacLimit(r, 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q, err := s.parseFeatureQuery(ctx, r, info, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	zero := 0
	q.overview = &zero
	var matched *int64
	if len(q.shapes) == 0 {
		if count, ok, err := s.countMatches(ctx, info, q.bbox, q.query, q.filter, q.tw); err == nil && ok {
			matched = &count
		}
	}
	w.Header().Set("Content-Type", "application/geo+json")
	sw := newStacWriter(w, matched, baseURL(r), limit)
	s.streamCollection(ctx, s.stacSinkFor(sw, q.shapes), info, q)
	sw.close(pageLink(r, limit, offset, sw.count))
}

type stacSearchBody struct {
	BBox        []float64       `json:"bbox"`
	Datetime    string          `json:"datetime"`
	Collections []string        `json:"collections"`
	Limit       int             `json:"limit"`
	Offset      int             `json:"offset"`
	FilterLang  string          `json:"filter-lang"`
	Filter      json.RawMessage `json:"filter"`
}

func (s *Server) stacSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	post := r.Method == http.MethodPost
	var body stacSearchBody
	if post {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "invalid search body", http.StatusBadRequest)
			return
		}
	}
	limit := 10
	if post && body.Limit > 0 {
		if body.Limit > stacMaxLimit {
			http.Error(w, "limit must be between 1 and "+strconv.Itoa(stacMaxLimit), http.StatusBadRequest)
			return
		}
		limit = body.Limit
	} else if !post {
		var err error
		if limit, err = parseStacLimit(r, 10); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	offset := 0
	if post && body.Offset > 0 {
		offset = body.Offset
	} else {
		var err error
		if offset, err = parseOffset(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var bbox *[4]float64
	if post && len(body.BBox) == 4 {
		bbox = &[4]float64{body.BBox[0], body.BBox[1], body.BBox[2], body.BBox[3]}
	} else {
		var err error
		if bbox, err = parseBBox(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var tw timeWindow
	if post && body.Datetime != "" {
		lo, hi, err := parseDatetime(body.Datetime)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tw = timeWindow{lo: lo, hi: hi, set: true}
	} else {
		var err error
		if tw, err = parseTimeWindow(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var filter cql.Expr
	if post {
		if len(body.Filter) > 0 {
			var err error
			if body.FilterLang == "cql2-text" {
				var text string
				if err := json.Unmarshal(body.Filter, &text); err != nil {
					http.Error(w, "filter must be a string for cql2-text", http.StatusBadRequest)
					return
				}
				filter, err = cql.Parse(text)
			} else {
				filter, err = cql.ParseJSON(body.Filter)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	} else {
		cloned := r.Clone(ctx)
		values := cloned.URL.Query()
		values.Del("collections")
		cloned.URL.RawQuery = values.Encode()
		var err error
		if filter, err = requestFilter(cloned); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	extracted, spatials, err := cql.ExtractSpatial(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter = extracted
	shapes := spatialShapes(spatials)
	merged, ok := mergeBBox(bbox, shapes)
	if !ok {
		w.Header().Set("Content-Type", "application/geo+json")
		sw := newStacWriter(w, nil, baseURL(r), limit)
		sw.close("")
		return
	}
	bbox = merged
	names := body.Collections
	if !post {
		if value := r.URL.Query().Get("collections"); value != "" {
			names = parseList(value)
		}
	}
	var infos []store.CollectionInfo
	if len(names) == 0 {
		if infos, err = s.Store.Collections(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		for _, name := range names {
			info, err := s.Store.Collection(ctx, name)
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "unknown collection "+name, http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			infos = append(infos, info)
		}
	}
	var matched *int64
	if bbox == nil && len(shapes) == 0 && filter == nil && !tw.set {
		var total int64
		for _, info := range infos {
			if count, ok, err := s.countMatches(ctx, info, nil, "", nil, timeWindow{}); err == nil && ok {
				total += count
			}
		}
		matched = &total
	}
	var members []unionMember
	for _, info := range infos {
		if info.Kind == "view" {
			vm, err := s.viewMembers(ctx, info)
			if err != nil {
				continue
			}
			members = append(members, vm...)
		} else {
			members = append(members, unionMember{info: info})
		}
	}
	q := featureQuery{bbox: bbox, tw: tw, filter: filter, shapes: shapes, offset: offset, limit: limit}
	zero := 0
	q.overview = &zero
	w.Header().Set("Content-Type", "application/geo+json")
	sw := newStacWriter(w, matched, baseURL(r), limit)
	s.streamUnion(ctx, members, s.stacSinkFor(sw, shapes), q.bbox, q.query, q.filter, q.offset, q.overview, q.tw)
	sw.close(stacNextLink(r, post, body, limit, offset, sw.count))
}

func stacNextLink(r *http.Request, post bool, body stacSearchBody, limit, offset, count int) string {
	if count < limit {
		return ""
	}
	if !post {
		return pageLink(r, limit, offset, count)
	}
	values := url.Values{}
	if len(body.BBox) == 4 {
		values.Set("bbox", strings.Join([]string{
			strconv.FormatFloat(body.BBox[0], 'f', -1, 64),
			strconv.FormatFloat(body.BBox[1], 'f', -1, 64),
			strconv.FormatFloat(body.BBox[2], 'f', -1, 64),
			strconv.FormatFloat(body.BBox[3], 'f', -1, 64),
		}, ","))
	}
	if body.Datetime != "" {
		values.Set("datetime", body.Datetime)
	}
	if len(body.Collections) > 0 {
		values.Set("collections", strings.Join(body.Collections, ","))
	}
	if len(body.Filter) > 0 {
		values.Set("filter", string(body.Filter))
		if body.FilterLang != "" {
			values.Set("filter-lang", body.FilterLang)
		}
	}
	values.Set("limit", strconv.Itoa(limit))
	values.Set("offset", strconv.Itoa(offset+count))
	return "/stac/search?" + values.Encode()
}
