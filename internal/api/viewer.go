package api

import (
	"encoding/json"
	"html/template"
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

var viewerPage = template.Must(template.New("viewer").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>postfgb · RustGIS</title>
<style>
*{box-sizing:border-box}
html,body{margin:0;height:100%;background:#0b0b0d;color:#e5e5e5;font:12px/1.4 ui-sans-serif,system-ui,sans-serif}
#app{display:flex;flex-direction:column;height:100%}
#bar{display:flex;align-items:center;gap:10px;padding:6px 10px;background:#18181b;border-bottom:1px solid #27272a;font:11px ui-monospace,monospace;color:#94a3b8}
#bar b{color:#e5e5e5;font:600 12px ui-sans-serif,system-ui,sans-serif}
#bar a{color:#38bdf8;text-decoration:none}
#bar a:hover{text-decoration:underline}
#bar .spacer{flex:1}
iframe{border:0;width:100%;flex:1;background:#0b0b0d}
</style>
</head>
<body>
<div id="app">
  <div id="bar">
    <b>postfgb</b>
    <span>{{.Host}}</span>
    <span class="spacer"></span>
    <a href="{{.Project}}" target="_blank" rel="noopener">project JSON</a>
    <a href="{{.Open}}" target="_blank" rel="noopener">open in RustGIS ↗</a>
  </div>
  <iframe src="{{.Embed}}" title="RustGIS" allow="fullscreen; clipboard-write" referrerpolicy="strict-origin-when-cross-origin"></iframe>
</div>
</body>
</html>
`))

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	project := base + "/viewer.rgis"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	viewerPage.Execute(w, struct {
		Host    string
		Project string
		Open    string
		Embed   string
	}{
		Host:    r.Host,
		Project: project,
		Open:    rustgisWeb + "?project=" + url.QueryEscape(project),
		Embed:   rustgisWeb + "?embed&project=" + url.QueryEscape(project),
	})
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
