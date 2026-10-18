package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

const (
	rustgisWeb           = "https://web.rustgis.app/"
	projectCameraWorld   = 40075016.685578488 / 512
	projectOverviewScale = 1000000.0
	projectDetailScale   = 140000.0
)

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	project := baseURL(r) + "/viewer.rgis"
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, rustgisWeb+"?embed&project="+url.QueryEscape(project), http.StatusFound)
}

func (s *Server) viewerProject(w http.ResponseWriter, r *http.Request) {
	infos, err := s.Store.Collections(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := baseURL(r)
	layers := []map[string]any{}
	children := []map[string]any{}
	next := 1
	for _, info := range infos {
		title := info.Name
		if info.Title != nil && *info.Title != "" {
			title = *info.Title
		}
		geometry := s.collectionGeometryType(r, info)
		if geometry == "" {
			continue
		}
		children = append(children, map[string]any{
			"layer":      next,
			"collection": info.Name,
			"source_url": base + "/collections/" + url.PathEscape(info.Name) + "/items",
		})
		layers = append(layers, s.projectLayers(r.Context(), base, info, title, geometry, &next)...)
	}
	centerX, centerY, res := projectCameraWorld, 0.0, projectCameraWorld
	if minX, minY, maxX, maxY, ok := catalogExtent(infos); ok {
		centerX = mercatorMeters((minX + maxX) / 2)
		centerY = mercatorMeters(math.Log(math.Tan((90+math.Min(math.Max((minY+maxY)/2, -85.05), 85.05))*math.Pi/360)) / (math.Pi / 180))
		res = math.Max(mercatorMeters(maxX-minX), mercatorMeters(maxY-minY)*1.4) / 512
		if res <= 0 || math.IsInf(res, 0) || math.IsNaN(res) {
			res = projectCameraWorld
		}
	}
	groups := []map[string]any{}
	if len(children) > 0 {
		groups = append(groups, map[string]any{
			"id":       1,
			"name":     "postfgb",
			"url":      base + "/collections",
			"kind":     "ogcapi",
			"expanded": false,
			"children": children,
		})
	}
	project := map[string]any{
		"format":     1,
		"name":       "postfgb",
		"active_map": 0,
		"maps": []map[string]any{{
			"name":           "postfgb",
			"layers":         layers,
			"service_groups": groups,
			"basemap":        "Paper",
			"projection":     "WebMercator",
			"next_layer_id":  next,
			"camera": map[string]any{
				"center": map[string]float64{"x": centerX, "y": centerY},
				"res":    res,
			},
		}},
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `inline; filename="postfgb.rgis"`)
	json.NewEncoder(w).Encode(project)
}

func mercatorMeters(degrees float64) float64 {
	return degrees * 20037508.342789244 / 180
}

func catalogExtent(infos []store.CollectionInfo) (minX, minY, maxX, maxY float64, ok bool) {
	minX, minY, maxX, maxY = math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, info := range infos {
		if info.MinX == nil || info.MinY == nil || info.MaxX == nil || info.MaxY == nil {
			continue
		}
		ok = true
		minX, minY = math.Min(minX, *info.MinX), math.Min(minY, *info.MinY)
		maxX, maxY = math.Max(maxX, *info.MaxX), math.Max(maxY, *info.MaxY)
	}
	return minX, minY, maxX, maxY, ok
}

func (s *Server) collectionGeometryType(r *http.Request, info store.CollectionInfo) string {
	collection := info.Name
	if info.Kind == "view" {
		names, err := s.Store.ViewMemberNames(r.Context(), info)
		if err != nil || len(names) == 0 {
			return ""
		}
		collection = names[0]
	}
	metas, err := s.Store.ChunkMetas(r.Context(), collection, nil, 1, 0, 0)
	if err != nil || len(metas) == 0 {
		return ""
	}
	row, err := s.Store.ChunkByID(r.Context(), collection, metas[0].ID)
	if err != nil {
		return ""
	}
	c, err := chunk.Open(row.Data)
	if err != nil {
		return ""
	}
	switch c.Header().GeometryType() {
	case flattypes.GeometryTypePoint, flattypes.GeometryTypeMultiPoint:
		return "Point"
	case flattypes.GeometryTypeLineString, flattypes.GeometryTypeMultiLineString:
		return "Polyline"
	case flattypes.GeometryTypePolygon, flattypes.GeometryTypeMultiPolygon:
		return "Polygon"
	}
	features, err := c.SearchAll()
	if err != nil {
		return ""
	}
	for _, f := range features {
		if g := c.GeoJSON(f).Geometry; g != nil {
			return geometryClass(g.Type)
		}
	}
	return ""
}

func geometryClass(typ string) string {
	switch strings.TrimPrefix(typ, "Multi") {
	case "Point":
		return "Point"
	case "LineString":
		return "Polyline"
	}
	return "Polygon"
}

type projectLayerSpec struct {
	label string
	query string
	min   any
	max   any
}

func (s *Server) projectLayers(ctx context.Context, base string, info store.CollectionInfo, title, geometry string, next *int) []map[string]any {
	var specs []projectLayerSpec
	if s.hasTier(ctx, info.Name, 2) {
		specs = append(specs, projectLayerSpec{label: " · 1%", query: "?overview=2", max: projectOverviewScale})
	}
	if s.hasTier(ctx, info.Name, 1) {
		specs = append(specs, projectLayerSpec{label: " · 10%", query: "?overview=1", min: projectOverviewScale, max: projectDetailScale})
	}
	specs = append(specs, projectLayerSpec{min: projectDetailScale})
	layers := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		layers = append(layers, map[string]any{
			"id":      *next,
			"name":    title + spec.label,
			"visible": false,
			"dataset": map[string]any{
				"name":          title + spec.label,
				"geometry_type": geometry,
				"fields":        []any{},
				"features":      []any{},
			},
			"renderer": map[string]any{"Single": map[string]any{
				"fill": []int{80, 160, 240, 120}, "stroke": []int{60, 120, 180, 255},
				"stroke_width": 0.7, "size": 3.0, "marker": "Circle",
			}},
			"labels": map[string]any{
				"enabled": false, "field": "name", "size": 10.0,
				"color": []int{43, 43, 43, 255}, "halo": true, "bold": false,
				"min_scale": nil, "max_scale": nil, "closest": nil, "farthest": nil,
				"placement": "Auto",
			},
			"transparency": 0.0,
			"expanded":     true,
			"selectable":   true,
			"min_scale":    spec.min,
			"max_scale":    spec.max,
			"source": map[string]any{
				"path": base + "/collections/" + url.PathEscape(info.Name) + "/items.fgb" + spec.query,
			},
		})
		*next++
	}
	return layers
}

func (s *Server) hasTier(ctx context.Context, collection string, tier int) bool {
	metas, err := s.Store.ChunkMetas(ctx, collection, nil, 1, tier, 0)
	return err == nil && len(metas) > 0
}
