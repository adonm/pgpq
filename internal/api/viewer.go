package api

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/adonm/postfgb/internal/store"
)

const (
	rustgisWeb         = "https://web.rustgis.app/"
	projectLayerMax    = 2000000
	projectTileMaxZoom = 16
	projectCameraWorld = 40075016.685578488 / 512
)

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	target := rustgisWeb + "?project=" + url.QueryEscape(baseURL(r)+"/viewer.rgis")
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) viewerProject(w http.ResponseWriter, r *http.Request) {
	infos, err := s.Store.Collections(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := baseURL(r)
	layers := []map[string]any{}
	tiles := []map[string]any{}
	next := 1
	for _, info := range infos {
		title := info.Name
		if info.Title != nil && *info.Title != "" {
			title = *info.Title
		}
		escaped := url.PathEscape(info.Name)
		tiles = append(tiles, map[string]any{
			"name":        title + " (vector tiles)",
			"kind":        "vector",
			"url":         base + "/collections/" + escaped + "/tiles/WebMercatorQuad/{z}/{y}/{x}?f=mvt",
			"minZoom":     0,
			"maxZoom":     projectTileMaxZoom,
			"tileSize":    256,
			"attribution": "© OpenStreetMap contributors",
			"visible":     false,
			"opacity":     1.0,
			"overlay":     true,
			"layers":      []string{info.Name},
		})
		if s.projectFeatureCount(r, info) > projectLayerMax {
			continue
		}
		layers = append(layers, map[string]any{
			"id":      next,
			"name":    title,
			"visible": false,
			"dataset": map[string]any{
				"name":          title,
				"geometry_type": "None",
				"fields":        []any{},
				"features":      []any{},
				"source":        info.Name + ".geojson",
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
			"source": map[string]any{
				"path": base + "/collections/" + escaped + "/items?f=flatgeobuf",
			},
		})
		next++
	}
	project := map[string]any{
		"format":     1,
		"name":       "postfgb",
		"active_map": 0,
		"maps": []map[string]any{{
			"name":          "postfgb",
			"layers":        layers,
			"tiles":         tiles,
			"basemap":       "Paper",
			"projection":    "WebMercator",
			"next_layer_id": next,
			"camera": map[string]any{
				"center": map[string]float64{"x": 0, "y": 0},
				"res":    projectCameraWorld,
			},
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `inline; filename="postfgb.rgis"`)
	json.NewEncoder(w).Encode(project)
}

func (s *Server) projectFeatureCount(r *http.Request, info store.CollectionInfo) int64 {
	if info.Kind != "view" {
		return info.FeatureCount
	}
	members, err := parseMembers(info)
	if err != nil {
		return projectLayerMax + 1
	}
	var total int64
	for _, member := range members {
		mi, err := s.Store.Collection(r.Context(), member.Collection)
		if err != nil {
			return projectLayerMax + 1
		}
		total += mi.FeatureCount
	}
	return total
}
